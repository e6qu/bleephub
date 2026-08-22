package bleephub

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"
)

const (
	maxConcurrentGitSSHConnections = 64
	gitSSHHandshakeTimeout         = 10 * time.Second
	// maxGitSSHAuthTries bounds how many key offers one connection may make
	// before the handshake is refused, matching OpenSSH's default of 6.
	maxGitSSHAuthTries = 6
)

// startGitSSH serves the standard SSH Git transport when BLEEPHUB_SSH_ADDR
// and BLEEPHUB_SSH_HOST_KEY are configured. The host key is required so a
// restarted durable server retains its SSH host identity instead of silently
// generating a different key for every process.
func (s *Server) startGitSSH(ctx context.Context) error {
	addr := strings.TrimSpace(os.Getenv("BLEEPHUB_SSH_ADDR"))
	if addr == "" {
		return nil
	}
	hostKey := os.Getenv("BLEEPHUB_SSH_HOST_KEY")
	if hostKey == "" {
		return errors.New("BLEEPHUB_SSH_ADDR is configured but BLEEPHUB_SSH_HOST_KEY is empty")
	}
	signer, err := ssh.ParsePrivateKey([]byte(hostKey))
	if err != nil {
		return fmt.Errorf("parse BLEEPHUB_SSH_HOST_KEY: %w", err)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for SSH Git transport on %s: %w", addr, err)
	}
	// The server config is built per connection in serveGitSSHConn so the
	// failed-attempt counter the callback closes over is per connection too.
	connectionSlots := make(chan struct{}, maxConcurrentGitSSHConnections)
	s.logger.Info().Str("addr", addr).Msg("bleephub SSH Git transport listening")
	// Closing the listener is what unblocks Accept, so shutdown closes it and
	// the accept loop reports a clean stop rather than an error.
	s.goBackground(func() {
		<-ctx.Done()
		_ = listener.Close()
	})
	s.goBackground(func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				if ctx.Err() != nil {
					s.logger.Info().Msg("SSH Git transport listener closed")
				} else {
					s.logger.Error().Err(acceptErr).Msg("SSH Git transport listener stopped")
				}
				return
			}
			select {
			case connectionSlots <- struct{}{}:
				s.goBackground(func() {
					defer func() { <-connectionSlots }()
					s.serveGitSSHConn(conn, signer)
				})
			default:
				s.logger.Warn().
					Str("remote_addr", conn.RemoteAddr().String()).
					Msg("SSH Git connection limit reached")
				_ = conn.Close()
			}
		}
	})
	return nil
}

func (s *Server) serveGitSSHConn(conn net.Conn, signer ssh.Signer) {
	defer func() { _ = conn.Close() }()
	// The SSH path runs outside recoverMiddleware and drives third-party
	// decoders (packp/packfile) over attacker-controlled bytes. A panic there
	// would unwind through the accept goroutine and kill the whole process,
	// taking every tenant's traffic down — so contain it to this connection.
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Error().Interface("panic", rec).Str("remote_addr", conn.RemoteAddr().String()).Msg("recovered panic in SSH Git connection")
		}
	}()
	if err := conn.SetDeadline(s.currentTime().Add(gitSSHHandshakeTimeout)); err != nil {
		s.logger.Debug().Err(err).Msg("set SSH Git handshake deadline")
		return
	}
	config := &ssh.ServerConfig{
		// MaxAuthTries makes the ssh library disconnect after too many
		// attempts; the callback itself stays stateless so the granted
		// permissions derive only from the key in the current invocation.
		MaxAuthTries: maxGitSSHAuthTries,
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			user := s.store.LookupUserBySSHKey(key)
			if user == nil || user.Suspended {
				s.logger.Warn().
					Str("remote_addr", metadata.RemoteAddr().String()).
					Msg("SSH Git auth failed")
				return nil, errors.New("unknown SSH key")
			}
			return &ssh.Permissions{Extensions: map[string]string{"bleephub-user-id": fmt.Sprintf("%d", user.ID)}}, nil
		},
	}
	config.AddHostKey(signer)
	serverConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		s.logger.Debug().Err(err).Msg("SSH Git handshake rejected")
		return
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		s.logger.Debug().Err(err).Msg("clear SSH Git handshake deadline")
		return
	}
	defer func() { _ = serverConn.Close() }()
	go ssh.DiscardRequests(requests)
	// The user ID extension was stamped by the public-key callback below, so
	// an unparseable value means the connection state is corrupt; fail closed
	// rather than resolve whatever user happens to hold ID 0.
	userID, err := parseGitSSHUserID(serverConn.Permissions.Extensions["bleephub-user-id"])
	if err != nil {
		s.logger.Warn().Err(err).Msg("SSH Git connection dropped")
		return
	}
	user := s.store.GetUserByID(userID)
	if user == nil {
		return
	}
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only SSH session channels are supported")
			continue
		}
		channel, requests, openErr := newChannel.Accept()
		if openErr != nil {
			continue
		}
		// A Git SSH connection uses one session channel. Serving it inline
		// keeps the listener's connection bound meaningful: a client cannot
		// open an unbounded number of channel goroutines behind one admitted
		// TCP connection.
		s.serveGitSSHSession(channel, requests, user)
	}
}

func parseGitSSHUserID(value string) (int, error) {
	// strconv.Atoi is the fail-closed primitive: unlike fmt.Sscanf("%d"), it
	// rejects trailing garbage such as "12abc" rather than accepting 12.
	result, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse SSH user ID %q: %w", value, err)
	}
	return result, nil
}

func (s *Server) serveGitSSHSession(channel ssh.Channel, requests <-chan *ssh.Request, user *store.User) {
	defer func() { _ = channel.Close() }()
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = request.Reply(false, nil)
			return
		}
		service, owner, repoName, ok := parseGitSSHCommand(payload.Command)
		if !ok {
			_, _ = io.WriteString(channel.Stderr(), "Unsupported SSH command.\n")
			_ = request.Reply(false, nil)
			return
		}
		_ = request.Reply(true, nil)
		exitStatus := uint32(0)
		if err := s.runGitSSHService(channel, service, owner, repoName, user); err != nil {
			s.logger.Debug().Err(err).Str("service", service).Str("repo", owner+"/"+repoName).Msg("SSH Git request failed")
			_, _ = io.WriteString(channel.Stderr(), "Bleephub: "+err.Error()+"\n")
			exitStatus = 1
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exitStatus}))
		return
	}
}

func parseGitSSHCommand(command string) (service, owner, repo string, ok bool) {
	fields := strings.Fields(command)
	if len(fields) != 2 || (fields[0] != "git-upload-pack" && fields[0] != "git-receive-pack") {
		return "", "", "", false
	}
	path := strings.Trim(fields[1], "'\"")
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	owner, repo = splitRepoPath("/" + path)
	if owner == "" || repo == "" || strings.ContainsAny(path, "\\\n\r") {
		return "", "", "", false
	}
	return fields[0], owner, repo, true
}

// sshChannelReader deliberately does not implement io.Closer. go-git's
// receive-pack decoder retains the supplied reader as its packfile and closes
// it after ingestion; closing an SSH channel there would also close its output
// side before the required report-status response can be written. The buffered
// reader wrapped around it — needed so the shallow lines a push from a shallow
// clone starts with can be consumed a whole pkt-line at a time — hides Close as
// well, so the guarantee is stated here rather than depending on that.
type sshChannelReader struct{ io.Reader }

func (s *Server) runGitSSHService(channel ssh.Channel, service, owner, repoName string, user *store.User) error {
	repo := s.store.GetRepo(owner, repoName)
	stor := s.resolveGitRepo(owner, repoName)
	if repo == nil || stor == nil {
		return transport.ErrRepositoryNotFound
	}
	// SSH authenticates by public key, so the only credential the session can
	// carry is the user itself — there is no installation or user-to-server
	// token to intersect. The read decision still goes through the one choke
	// point, so a new arm added there cannot be missed here.
	ctx := contextWithUser(context.Background(), user)
	// A caller who cannot read the repository is answered exactly as for a
	// nonexistent one, so SSH does not become a private-repository existence
	// oracle (matching the git-HTTP behavior). A write refusal is only
	// distinguishable once read access is established.
	// Cloning reads repository CONTENTS, so gate on contents:read (matching the
	// git-HTTP path) rather than metadata:read — a caller with only metadata
	// visibility must not be able to pull the code.
	if !s.viewerHasRepoPermission(ctx, repo, store.ScopeContents, store.PermRead) {
		return transport.ErrRepositoryNotFound
	}
	if service == "git-receive-pack" && !s.viewerCanPushRepo(ctx, repo) {
		return errors.New("repository write access denied")
	}
	if service == "git-upload-pack" {
		// The advertisement and the negotiation are the shared ones in
		// git_uploadpack.go, so SSH and smart HTTP cannot drift apart on which
		// capabilities they promise or how they answer a deepening request.
		info, err := gitUploadPackAdvertisement(stor)
		if err != nil {
			return err
		}
		advertiseDefaultBranchSymref(info, repo)
		if err := info.Encode(channel); err != nil {
			return err
		}
		requestReader := bufio.NewReader(channel)
		empty, err := flushOnlyGitRequest(requestReader)
		if err != nil {
			return err
		}
		if empty {
			return pktline.NewEncoder(channel).Flush()
		}
		_, err = serveGitUploadPack(context.Background(), stor, requestReader, channel)
		return err
	}
	session, err := s.newGitReceivePackSession(owner, repoName, stor)
	if err != nil {
		return err
	}
	info, err := session.AdvertisedReferencesContext(context.Background())
	if err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return err
	}
	if info != nil {
		advertiseDefaultBranchSymref(info, repo)
		if err := info.Encode(channel); err != nil {
			return err
		}
	} else if err := pktline.NewEncoder(channel).Flush(); err != nil {
		return err
	}
	pushReader := bufio.NewReader(sshChannelReader{Reader: channel})
	if err := skipPushedShallowLines(pushReader); err != nil {
		return err
	}
	request := packp.NewReferenceUpdateRequest()
	if err := request.Decode(pushReader); err != nil {
		return err
	}
	result, err := s.applyReceivePack(ctx, repo, stor, session, request)
	if err != nil {
		return err
	}
	s.afterGitReceivePack(repo, user, appliedPushCommands(request, result), s.externalURL)
	if result != nil {
		return result.Encode(channel)
	}
	return nil
}

// flushOnlyGitRequest recognizes the valid upload-pack request a Git client
// sends after advertising an empty repository. There are no objects to want,
// so the request consists solely of the pkt-line flush marker. go-git's
// request decoder intentionally expects at least one want line and cannot
// represent this protocol-valid case.
func flushOnlyGitRequest(reader *bufio.Reader) (bool, error) {
	header, err := reader.Peek(4)
	if err != nil {
		return false, err
	}
	return string(header) == "0000", nil
}

// metaSSHHostKeys returns the instance's SSH host-key material for GET /meta:
// the SHA256 fingerprints map (keyed SHA256_<ALG>, value without the "SHA256:"
// prefix, matching github) and the authorized-key lines. Both are empty when no
// host key is configured (BLEEPHUB_SSH_HOST_KEY unset), which is spec-valid —
// the /meta ssh_key_fingerprints/ssh_keys members are optional.
func metaSSHHostKeys() (fingerprints map[string]string, authorized []string) {
	fingerprints, authorized = map[string]string{}, []string{}
	raw := os.Getenv("BLEEPHUB_SSH_HOST_KEY")
	if raw == "" {
		return fingerprints, authorized
	}
	signer, err := ssh.ParsePrivateKey([]byte(raw))
	if err != nil {
		return fingerprints, authorized
	}
	pub := signer.PublicKey()
	alg := "OTHER"
	switch t := pub.Type(); {
	case strings.Contains(t, "ed25519"):
		alg = "ED25519"
	case strings.Contains(t, "rsa"):
		alg = "RSA"
	case strings.Contains(t, "ecdsa"):
		alg = "ECDSA"
	case strings.Contains(t, "dss"):
		alg = "DSA"
	}
	fingerprints["SHA256_"+alg] = strings.TrimPrefix(ssh.FingerprintSHA256(pub), "SHA256:")
	authorized = []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))}
	return fingerprints, authorized
}
