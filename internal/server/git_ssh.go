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
	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"
)

const (
	maxConcurrentGitSSHConnections = 64
	gitSSHHandshakeTimeout         = 10 * time.Second
	// maxGitSSHAuthTries caps key offers per connection, matching OpenSSH's default.
	maxGitSSHAuthTries = 6
)

// startGitSSH serves the SSH Git transport when BLEEPHUB_SSH_ADDR and
// BLEEPHUB_SSH_HOST_KEY are configured. The host key is required so a restarted
// server keeps its SSH host identity across processes.
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
	connectionSlots := make(chan struct{}, maxConcurrentGitSSHConnections)
	s.logger.Info().Str("addr", addr).Msg("bleephub SSH Git transport listening")
	// Shutdown closes the listener to unblock Accept.
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
	// The SSH path runs outside recoverMiddleware and drives third-party decoders
	// over attacker-controlled bytes; contain any panic to this connection so it
	// cannot kill the accept goroutine and the whole process.
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
		// The callback stays stateless: granted permissions derive only from the
		// key in the current invocation.
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
	// An unparseable user-ID extension means corrupt connection state; fail
	// closed rather than resolve whatever user holds ID 0.
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
		// Serve inline so a client cannot open unbounded channel goroutines
		// behind one admitted TCP connection.
		s.serveGitSSHSession(channel, requests, user)
	}
}

func parseGitSSHUserID(value string) (int, error) {
	// Atoi rejects trailing garbage ("12abc"), unlike fmt.Sscanf("%d").
	result, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse SSH user ID %q: %w", value, err)
	}
	return result, nil
}

func (s *Server) serveGitSSHSession(channel ssh.Channel, requests <-chan *ssh.Request, user *store.User) {
	defer func() { _ = channel.Close() }()
	// A protocol-v2 client states so in GIT_PROTOCOL, which OpenSSH forwards as
	// an env request before the exec request; collect it here to negotiate the
	// same version smart HTTP does through a header.
	protocol := ""
	for request := range requests {
		if request.Type == "env" {
			var variable struct{ Name, Value string }
			if err := ssh.Unmarshal(request.Payload, &variable); err != nil {
				_ = request.Reply(false, nil)
				return
			}
			if variable.Name == "GIT_PROTOCOL" {
				protocol = variable.Value
			}
			_ = request.Reply(true, nil)
			continue
		}
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
		if err := s.runGitSSHService(channel, service, owner, repoName, user, gitProtocolV2Requested(protocol)); err != nil {
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

func (s *Server) runGitSSHService(channel ssh.Channel, service, owner, repoName string, user *store.User, protocolV2 bool) error {
	// Shared with the smart-HTTP lane, so `<repo>.wiki.git` names the same
	// storage over SSH as over HTTP.
	target := s.resolveGitTarget(owner, repoName)
	if !target.exists() {
		return transport.ErrRepositoryNotFound
	}
	repo := target.repo
	ctx := contextWithUser(context.Background(), user)
	// Answer an unreadable repo exactly as a nonexistent one, so SSH is no
	// private-repo existence oracle; a write refusal is distinguishable only
	// after read access is established. Cloning reads CONTENTS, so gate on
	// contents:read (not metadata:read) as the git-HTTP path does.
	if !s.viewerHasRepoPermission(ctx, repo, store.ScopeContents, store.PermRead) {
		return transport.ErrRepositoryNotFound
	}
	if service == "git-receive-pack" && !s.viewerMayWriteGitTarget(ctx, target) {
		return errors.New("repository write access denied")
	}
	if !s.openGitTarget(target) {
		return transport.ErrRepositoryNotFound
	}
	stor := target.stor
	if service == "git-upload-pack" {
		stor = gitStorerWithPackReuse(ctx, target.storageName, stor)
		if protocolV2 {
			// The channel stays open, so the v2 loop serves commands until the
			// client stops — one connection carries both ls-refs and the fetch.
			if err := writeGitV2CapabilityAdvertisement(channel); err != nil {
				return err
			}
			result, err := serveGitProtocolV2(context.Background(), stor, target.defaultBranch, bufio.NewReader(channel), channel, false)
			if result.sessionID != "" {
				s.logger.Debug().
					Str("repo", owner+"/"+repoName).
					Str("client_session_id", result.sessionID).
					Str("server_session_id", gitServerSessionID()).
					Msg("git SSH upload-pack session")
			}
			return err
		}
		// Shared with git_uploadpack.go so SSH and smart HTTP cannot drift on
		// advertised capabilities or deepening.
		info, err := gitUploadPackAdvertisement(stor)
		if err != nil {
			return err
		}
		advertiseDefaultBranchSymref(info, target.defaultBranch)
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
	// Shared with git_receivepack.go so SSH and smart HTTP cannot drift on
	// advertised capabilities or how they answer a push.
	info, err := gitReceivePackAdvertisement(stor)
	if err != nil {
		return err
	}
	advertiseDefaultBranchSymref(info, target.defaultBranch)
	if err := info.Encode(channel); err != nil {
		return err
	}
	// Read straight off the channel: the decoder never closes it, so the output
	// side stays open for the report the client awaits.
	request, err := decodeGitReceiveRequest(bufio.NewReader(channel))
	if err != nil {
		return err
	}
	outcome, err := s.applyGitReceivePack(ctx, target, request)
	if err != nil {
		return err
	}
	if target.wiki {
		s.afterWikiReceivePack(repo, user, outcome.applied, s.externalURL)
	} else {
		s.afterGitReceivePack(repo, user, outcome.applied, s.externalURL)
	}
	return writeGitReceivePackResponse(channel, request, outcome)
}

// flushOnlyGitRequest recognizes the flush-only upload-pack request a client
// sends against an empty repo (no wants). go-git's decoder requires at least
// one want line and cannot represent this protocol-valid case.
func flushOnlyGitRequest(reader *bufio.Reader) (bool, error) {
	header, err := reader.Peek(4)
	if err != nil {
		return false, err
	}
	return string(header) == "0000", nil
}

// metaSSHHostKeys returns the SSH host-key material for GET /meta: the SHA256
// fingerprints map (keyed SHA256_<ALG>, value without the "SHA256:" prefix, as
// github does) and the authorized-key lines. Both are empty when no host key is
// configured — the /meta members are optional.
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
