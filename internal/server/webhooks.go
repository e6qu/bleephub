package bleephub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- GitHub's legacy X-Hub-Signature compatibility header requires HMAC-SHA1
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"gopkg.in/yaml.v3"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

func computeHMACSignature(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func computeHMACSignatureSHA1(secret string, payload []byte) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(payload)
	return "sha1=" + hex.EncodeToString(mac.Sum(nil))
}

const (
	// webhookDeliveryWorkers bounds in-flight deliveries so a hanging target
	// costs one worker, not a goroutine per event.
	webhookDeliveryWorkers = 16
	webhookDialTimeout     = 5 * time.Second
	webhookRequestTimeout  = 10 * time.Second
	webhookResolveTimeout  = 2 * time.Second
)

// webhookAddrBlocked reports whether a server-initiated fetch must never reach
// this IP. Loopback is the one permitted non-public range (on-prem/dev
// receivers); every other non-public address — metadata endpoint, link-local,
// RFC1918, IPv6 ULA, CGNAT, multicast, unspecified, broadcast — is refused
// unconditionally, with no operator switch to relax it.
func webhookAddrBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() {
		return false
	}
	if nonPublicIP(ip) {
		return true
	}
	// An IPv6 form tunnelling IPv4 reaches whatever that address reaches, so
	// check the embedded address on its own terms.
	if v4 := tunnelledIPv4(ip); v4 != nil {
		if v4.IsLoopback() {
			return false
		}
		if nonPublicIP(v4) {
			return true
		}
	}
	return false
}

// nonPublicIP is webhookAddrBlocked's per-address-family core.
func nonPublicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return true // 100.64.0.0/10, carrier-grade NAT
	}
	return false
}

// tunnelledIPv4 extracts the IPv4 address carried inside an IPv6 address that
// embeds one — ::ffff:a.b.c.d, ::a.b.c.d, and the NAT64 well-known prefix
// 64:ff9b::/96 — or nil when there is none.
func tunnelledIPv4(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	if len(ip) != net.IPv6len {
		return nil
	}
	nat64 := ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b
	if nat64 || isZeroBytes(ip[:12]) {
		if !isZeroBytes(ip[4:12]) && nat64 {
			return nil
		}
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	return nil
}

func isZeroBytes(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// parseWebhookTargetURL checks the resolution-free parts of a target URL —
// scheme, host presence, literal IP — and returns the parsed URL. Cheap enough
// to repeat on every delivery attempt.
func parseWebhookTargetURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("webhook URL %q is not a valid URL: %w", raw, err)
	}
	// Shared with repository import fetches, which allow http; the https-only
	// rule for delivery is enforced separately by webhookTargetRequiresHTTPS.
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("webhook URL scheme %q is not deliverable; use http or https", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("webhook URL %q names no host", raw)
	}
	if ip := net.ParseIP(host); ip != nil && webhookAddrBlocked(ip) {
		return nil, fmt.Errorf("webhook target %s is not a public address", ip)
	}
	return u, nil
}

// validateWebhookTargetURL is the config-time gate: shape check plus one name
// resolution, so a hostname pointing at private space is refused at store time.
// An unresolvable name is stored anyway — the dialed address is re-checked at
// delivery time, the only point that catches DNS rebinding.
func validateWebhookTargetURL(raw string) error {
	u, err := parseWebhookTargetURL(raw)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), webhookResolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		if webhookAddrBlocked(addr.IP) {
			return fmt.Errorf("webhook target %s resolves to %s, which is not a public address", host, addr.IP)
		}
	}
	return nil
}

// webhookDialControl is the delivery-time address gate, running between resolve
// and connect with the exact address about to be dialed — the only point where
// checked and reached cannot differ.
func webhookDialControl() func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("webhook target address %q is malformed: %w", address, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("webhook target address %q is not an IP address", address)
		}
		if webhookAddrBlocked(ip) {
			return fmt.Errorf("webhook delivery to %s refused: not a public address", ip)
		}
		return nil
	}
}

func newWebhookClient(insecureTLS bool) *http.Client {
	return newWebhookClientWithTimeout(insecureTLS, webhookRequestTimeout)
}

func newWebhookClientWithTimeout(insecureTLS bool, timeout time.Duration) *http.Client {
	transport := newAddressCheckedHTTPTransport(insecureTLS)
	return &http.Client{
		Timeout:   timeout,
		Transport: otelhttp.NewTransport(transport),
		// Do not follow redirects: a redirect could reach a second destination
		// the address gate never saw. Return the 3xx as the outcome.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// newAddressCheckedHTTPTransport is the shared server-side request boundary for
// URL fetchers; keeping it shared keeps webhook and source-import SSRF policy
// from drifting apart. Only Dialer.Control sees the address actually reached.
func newAddressCheckedHTTPTransport(insecureTLS bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   webhookDialTimeout,
		KeepAlive: 30 * time.Second,
		Control:   webhookDialControl(),
	}
	transport := &http.Transport{
		// No proxy: a proxied request dials the proxy, so the address gate would
		// inspect the proxy instead of the target.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if insecureTLS {
		// insecure_ssl=1 relaxes CA-chain and hostname verification;
		// VerifyConnection still rejects an absent or expired certificate, and
		// the dial-time gate and redirect block remain.
		// #nosec G402 -- chain/hostname verification is intentionally relaxed per opt-in hook config; residual validity check below.
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			// deepcode ignore TooPermissiveTrustManager: This is the opt-in webhook insecure_ssl=1 feature (GitHub exposes the same switch); it cannot exist without relaxing chain/hostname checks. VerifyConnection below still rejects absent or expired certificates and TLS is floored at 1.2, so the client is not a blanket accept-anything trust manager.
			InsecureSkipVerify: true, // custom validity check supplied via VerifyConnection
			VerifyConnection:   verifyWebhookPeerCertificateValidity,
		}
	} else if webhookDeliveryTestRoots != nil {
		// Test-only seam: trust the in-process httptest TLS receiver; nil in
		// production.
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: webhookDeliveryTestRoots}
	}
	return transport
}

// webhookDeliveryTestRoots is the test harness's trusted roots; nil in
// production.
var webhookDeliveryTestRoots *x509.CertPool

// verifyWebhookPeerCertificateValidity is the insecure_ssl VerifyConnection
// callback: it skips CA-chain and hostname checks but rejects a peer with no
// certificate or a leaf outside its validity window.
func verifyWebhookPeerCertificateValidity(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return fmt.Errorf("webhook TLS peer presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fmt.Errorf("webhook TLS peer certificate outside validity window %s..%s",
			leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	return nil
}

func (s *Server) webhookDeliveryClient(insecureTLS bool) *http.Client {
	s.webhookClientsOnce.Do(func() {
		s.webhookClients = [2]*http.Client{
			newWebhookClient(false),
			newWebhookClient(true),
		}
	})
	if insecureTLS {
		return s.webhookClients[1]
	}
	return s.webhookClients[0]
}

// webhookDispatcher runs deliveries on a fixed worker pool while preserving
// per-hook order: each hook has its own FIFO queue, drained by one worker at a
// time.
type webhookDispatcher struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queues map[string]*webhookQueue
	ready  []*webhookQueue
}

type webhookQueue struct {
	key     string
	pending []func()
	// queued marks the queue as on the ready list or being drained; it stops two
	// workers from reordering one hook's events.
	queued bool
}

func newWebhookDispatcher(workers int) *webhookDispatcher {
	d := &webhookDispatcher{queues: map[string]*webhookQueue{}}
	d.cond = sync.NewCond(&d.mu)
	for i := 0; i < workers; i++ {
		go d.work()
	}
	return d
}

func (d *webhookDispatcher) enqueue(key string, job func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.queues[key]
	if q == nil {
		q = &webhookQueue{key: key}
		d.queues[key] = q
	}
	q.pending = append(q.pending, job)
	if !q.queued {
		q.queued = true
		d.ready = append(d.ready, q)
		d.cond.Signal()
	}
}

func (d *webhookDispatcher) work() {
	for {
		d.mu.Lock()
		for len(d.ready) == 0 {
			d.cond.Wait()
		}
		q := d.ready[0]
		d.ready = d.ready[1:]
		d.mu.Unlock()
		d.drain(q)
	}
}

func (d *webhookDispatcher) drain(q *webhookQueue) {
	for {
		d.mu.Lock()
		if len(q.pending) == 0 {
			q.queued = false
			delete(d.queues, q.key)
			d.mu.Unlock()
			return
		}
		job := q.pending[0]
		q.pending = q.pending[1:]
		d.mu.Unlock()
		job()
	}
}

func (s *Server) webhookDispatch() *webhookDispatcher {
	s.webhookPoolOnce.Do(func() {
		s.webhookPool = newWebhookDispatcher(webhookDeliveryWorkers)
	})
	return s.webhookPool
}

// webhookQueueKey identifies a hook's delivery-order domain. The owner is
// included so a reloaded hook cannot collide with a live one.
func webhookQueueKey(h *store.Webhook) string {
	global := ""
	if h.Global {
		global = "global"
	}
	return h.RepoKey + "\x00" + h.OrgLogin + "\x00" + h.MarketplaceSlug + "\x00" + global + "\x00" + strconv.Itoa(h.ID)
}

// appWebhookPseudoHook is the Webhook view of a GitHub App's webhook config; the
// negative id marks an app-level delivery throughout the delivery path.
func appWebhookPseudoHook(app *store.App) *store.Webhook {
	return &store.Webhook{
		ID:     -app.ID,
		URL:    app.WebhookURL,
		Secret: app.WebhookSecret,
		Events: app.WebhookEvents,
		Active: app.WebhookActive,
	}
}

func appWebhookQueueKey(app *store.App) string {
	return "app\x00" + strconv.Itoa(app.ID)
}

// enqueueWebhookJob schedules work on the delivery pool, ordered behind anything
// already queued for the same key.
func (s *Server) enqueueWebhookJob(key string, job func()) {
	s.webhookDispatch().enqueue(key, job)
}

// enqueueWebhookDelivery is the asynchronous form of deliverWebhook; every fan-
// out goes through the pool rather than a bare `go` so an event storm cannot
// spawn a goroutine per hook per event.
func (s *Server) enqueueWebhookDelivery(hook *store.Webhook, event, action string, payloadBytes []byte) {
	queued := s.store.SnapshotHook(hook)
	s.enqueueWebhookJob(webhookQueueKey(queued), func() {
		s.deliverWebhook(queued, event, action, payloadBytes)
	})
}

// redeliverWebhook re-runs one recorded delivery and stores the new attempt.
func (s *Server) redeliverWebhook(hook *store.Webhook, event, action, guid string, payloadBytes []byte) {
	hook = s.store.SnapshotHook(hook)
	delivery := s.doDeliverAttempt(hook, event, action, guid, payloadBytes, true)
	s.store.AddDelivery(delivery)
	s.recordHookLastResponse(hook, delivery)
}

// recordHookLastResponse writes the attempt outcome to whichever hook table owns
// the hook (repository, organization, or global).
func (s *Server) recordHookLastResponse(hook *store.Webhook, delivery *store.WebhookDelivery) {
	switch {
	case hook.RepoKey != "":
		s.store.SetHookLastResponse(hook.RepoKey, hook.ID, deliveryLastResponse(delivery))
	case hook.OrgLogin != "":
		s.store.SetOrgHookLastResponse(hook.OrgLogin, hook.ID, deliveryLastResponse(delivery))
	case hook.Global:
		s.store.Mu.Lock()
		for _, stored := range s.store.EnterpriseSettings.GHESGlobalHooks {
			if stored.ID == hook.ID {
				stored.LastResponse = deliveryLastResponse(delivery)
				stored.UpdatedAt = s.store.CurrentTime()
				break
			}
		}
		s.store.PersistEnterpriseSettings()
		s.store.Mu.Unlock()
	}
}

// emitWebhookEvent dispatches an event to matching webhooks (non-blocking). For
// org-owned repos it also fans out to the owner org's hooks, as GitHub does.
func (s *Server) emitWebhookEvent(repoKey, eventType, action string, payload interface{}) {
	hooks := s.store.ListHooks(repoKey)
	if ownerLogin, _, found := strings.Cut(repoKey, "/"); found {
		if s.store.GetOrg(ownerLogin) != nil {
			hooks = append(hooks, s.store.ListOrgHooks(ownerLogin)...)
		}
	}

	// Org-owned repos carry a top-level `organization` object on every event.
	// Attached centrally here since the store lookup needs server access the
	// payload builders lack.
	if m, ok := payload.(map[string]interface{}); ok {
		if _, has := m["organization"]; !has {
			ownerLogin, _, found := strings.Cut(repoKey, "/")
			if found {
				if org := s.store.GetOrg(ownerLogin); org != nil {
					m["organization"] = orgWebhookPayload(org, s.publicOrigin())
				}
			}
		}
		s.triggerWorkflowsForWebhookEvent(repoKey, eventType, action, m)
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		s.logger.Error().Err(err).Str("repo", repoKey).Msg("failed to marshal webhook payload")
		return
	}

	for _, hook := range hooks {
		if !hook.Active {
			continue
		}
		if !hookMatchesEvent(hook, eventType) {
			continue
		}
		s.enqueueWebhookDelivery(hook, eventType, action, payloadBytes)
	}
}

// triggerWorkflowsForWebhookEvent couples webhook and Actions event production
// so a mutator cannot emit an event for hooks yet forget it triggers workflows.
func (s *Server) triggerWorkflowsForWebhookEvent(repoKey, eventType, action string, payload map[string]interface{}) {
	repo := s.store.GetRepoByFullName(repoKey)
	if repo == nil {
		return
	}
	ref := plumbing.NewBranchReferenceName(repo.DefaultBranch).String()
	switch eventType {
	case "push":
		if eventRef, _ := payload["ref"].(string); eventRef != "" {
			ref = eventRef
		}
	case "pull_request":
		if number := webhookPullRequestNumber(payload); number > 0 {
			ref = fmt.Sprintf("refs/pull/%d/merge", number)
		}
	case "pull_request_target":
		if pull, _ := payload["pull_request"].(map[string]interface{}); pull != nil {
			if base, _ := pull["base"].(map[string]interface{}); base != nil {
				if baseRef, _ := base["ref"].(string); baseRef != "" {
					ref = plumbing.NewBranchReferenceName(baseRef).String()
				}
			}
		}
	case "release":
		if release, _ := payload["release"].(map[string]interface{}); release != nil {
			if draft, _ := release["draft"].(bool); draft &&
				(action == "created" || action == "edited" || action == "deleted") {
				return
			}
			if tag, _ := release["tag_name"].(string); tag != "" {
				ref = plumbing.NewTagReferenceName(tag).String()
			}
		}
	case "deployment", "deployment_status":
		if deployment, _ := payload["deployment"].(map[string]interface{}); deployment != nil {
			if deploymentRef, _ := deployment["ref"].(string); deploymentRef != "" {
				owner, name, _ := store.SplitRepoFullName(repo.FullName)
				stor := s.store.GetGitStorage(owner, name)
				if stor != nil {
					if normalized, sha := resolveGitHubRefInput(stor, deploymentRef); sha != actions.ZeroCommitSha {
						ref = normalized
					}
				}
			}
		}
	}
	s.triggerWorkflowsForEvent(repoKey, eventType, action, ref, payload)
}

func hookMatchesEvent(hook *store.Webhook, eventType string) bool {
	for _, e := range hook.Events {
		if e == eventType || e == "*" {
			return true
		}
	}
	return false
}

// deliverWebhook sends an HTTP POST with retries (3 attempts, exponential backoff).
func (s *Server) deliverWebhook(hook *store.Webhook, event, action string, payloadBytes []byte) {
	hook = s.store.SnapshotHook(hook)
	backoffs := []time.Duration{0, 1 * time.Second, 5 * time.Second}
	if _, err := parseWebhookTargetURL(hook.URL); err != nil {
		// A refused target won't become deliverable by waiting: record once.
		backoffs = backoffs[:1]
	}

	for attempt, backoff := range backoffs {
		if attempt > 0 {
			time.Sleep(backoff)
		}

		// Each attempt is its own delivery record with a unique GUID; a retry is
		// a fresh delivery flagged as a redelivery.
		guid := uuid.New().String()
		delivery := s.doDeliverAttempt(hook, event, action, guid, payloadBytes, attempt > 0)
		s.store.AddDelivery(delivery)
		s.recordHookLastResponse(hook, delivery)

		if delivery.StatusCode >= 200 && delivery.StatusCode < 300 {
			return
		}
	}
}

// deliveryLastResponse maps a delivery attempt to the hook.last_response shape.
func deliveryLastResponse(d *store.WebhookDelivery) *store.HookLastResponse {
	msg := http.StatusText(d.StatusCode)
	if d.StatusCode >= 200 && d.StatusCode < 300 {
		msg = "OK"
	} else if d.StatusCode == 0 {
		msg = "failed to connect"
	}
	return &store.HookLastResponse{
		Code:    d.StatusCode,
		Status:  deliveryStatus(d.StatusCode),
		Message: msg,
	}
}

// doDeliverAttempt performs a single webhook delivery attempt.
func (s *Server) doDeliverAttempt(hook *store.Webhook, event, action, guid string, payloadBytes []byte, redelivery bool) *store.WebhookDelivery {
	hook = s.store.SnapshotHook(hook)
	start := time.Now()

	// content_type=form wraps the JSON in a `payload` form field and signs
	// THAT urlencoded body, not the raw JSON; content_type=json sends it
	// verbatim. hook.ContentType picks which.
	contentType := "application/json"
	bodyBytes := payloadBytes
	if hook.ContentType == "form" {
		contentType = "application/x-www-form-urlencoded"
		bodyBytes = []byte(url.Values{"payload": {string(payloadBytes)}}.Encode())
	}

	reqHeaders := map[string]string{
		"Content-Type":      contentType,
		"User-Agent":        "GitHub-Hookshot/bleephub",
		"X-GitHub-Event":    event,
		"X-GitHub-Delivery": guid,
		"X-GitHub-Hook-ID":  strconv.Itoa(hook.ID),
	}
	if hook.Secret != "" {
		reqHeaders["X-Hub-Signature-256"] = computeHMACSignature(hook.Secret, bodyBytes)
		reqHeaders["X-Hub-Signature"] = computeHMACSignatureSHA1(hook.Secret, bodyBytes)
	}
	// Installation-target headers name the resource owning the hook: app-bound
	// (HookID < 0) → integration, org → org id, repository → repo id.
	switch {
	case hook.MarketplaceSlug != "":
		// Marketplace webhooks are listing-scoped and advertise no target.
	case hook.ID < 0:
		reqHeaders["X-GitHub-Hook-Installation-Target-Type"] = "integration"
		reqHeaders["X-GitHub-Hook-Installation-Target-ID"] = strconv.Itoa(-hook.ID)
	case hook.OrgLogin != "":
		reqHeaders["X-GitHub-Hook-Installation-Target-Type"] = "organization"
		if org := s.store.GetOrg(hook.OrgLogin); org != nil {
			reqHeaders["X-GitHub-Hook-Installation-Target-ID"] = strconv.Itoa(org.ID)
		}
	default:
		reqHeaders["X-GitHub-Hook-Installation-Target-Type"] = "repository"
		parts := actions.SplitRepoKeyParts(hook.RepoKey)
		if parts[1] != "" {
			if repo := s.store.GetRepo(parts[0], parts[1]); repo != nil {
				reqHeaders["X-GitHub-Hook-Installation-Target-ID"] = strconv.Itoa(repo.ID)
			}
		}
	}

	// undelivered records an attempt that never reached the network.
	undelivered := func(err error) *store.WebhookDelivery {
		return &store.WebhookDelivery{
			HookID:      hook.ID,
			TargetURL:   hook.URL,
			GUID:        guid,
			Event:       event,
			Action:      action,
			StatusCode:  0,
			Duration:    time.Since(start).Seconds(),
			Request:     &store.DeliveryRequest{Headers: reqHeaders, Payload: json.RawMessage(payloadBytes)},
			Response:    &store.DeliveryResponse{StatusCode: 0, Body: err.Error()},
			Redelivery:  redelivery,
			DeliveredAt: time.Now(),
		}
	}

	// Every delivery path funnels through here, so re-check the stored target:
	// a hook configured before a rule tightened is refused just the same.
	if _, err := parseWebhookTargetURL(hook.URL); err != nil {
		s.logger.Warn().Err(err).Int("hook_id", hook.ID).Msg("webhook delivery refused")
		return undelivered(err)
	}

	httpReq, err := http.NewRequest("POST", hook.URL, bytes.NewReader(bodyBytes))
	if err != nil {
		return undelivered(err)
	}

	for k, v := range reqHeaders {
		httpReq.Header.Set(k, v)
	}

	// #nosec G704 -- webhooks deliver to operator-configured URLs by design;
	// parseWebhookTargetURL above refuses non-public targets (loopback aside),
	// and the dial-time gate re-checks the address actually reached.
	resp, err := s.webhookDeliveryClient(hook.InsecureSSL == "1").Do(httpReq)
	elapsed := time.Since(start).Seconds()

	delivery := &store.WebhookDelivery{
		HookID:      hook.ID,
		TargetURL:   hook.URL,
		GUID:        guid,
		Event:       event,
		Action:      action,
		Redelivery:  redelivery,
		DeliveredAt: time.Now(),
		Duration:    elapsed,
		Request:     &store.DeliveryRequest{Headers: reqHeaders, Payload: json.RawMessage(payloadBytes)},
	}

	if err != nil {
		delivery.StatusCode = 0
		delivery.Response = &store.DeliveryResponse{StatusCode: 0, Body: err.Error()}
		return delivery
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	respHeaders := make(map[string]string)
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}

	delivery.StatusCode = resp.StatusCode
	delivery.Response = &store.DeliveryResponse{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       string(respBody),
	}

	return delivery
}

// triggerWorkflowsForEvent triggers matching workflows for a concrete event.
// action is the activity type; payload becomes the run's github.event context
// and feeds the trigger filters.
func (s *Server) triggerWorkflowsForEvent(repoKey, eventType, action, ref string, payload map[string]interface{}) {
	parts := actions.SplitRepoKeyParts(repoKey)
	if parts[0] == "" {
		return
	}
	if !s.actionsEnabledForRepo(repoKey) {
		s.logger.Info().Str("repo", repoKey).Str("event", eventType).
			Msg("workflow trigger skipped by Actions permissions")
		return
	}
	if eventType == "push" && pushPayloadSkipsActions(payload) {
		s.logger.Info().Str("repo", repoKey).Str("ref", ref).
			Msg("workflow trigger skipped by commit message directive")
		return
	}

	stor := s.store.GetGitStorage(parts[0], parts[1])
	if stor == nil {
		return
	}

	refs, err := workflowRefsForEvent(stor, repoKey, eventType, ref, payload)
	if err != nil {
		s.logger.Error().Err(err).
			Str("repo", repoKey).
			Str("event", eventType).
			Str("ref", ref).
			Msg("workflow trigger rejected")
		return
	}
	sha := refs.runSha

	workflowFiles := actions.ListWorkflowFilesAtRef(stor, refs.definitionRef)
	if len(workflowFiles) == 0 {
		return
	}

	ref = refs.runRef
	ev := s.buildTriggerEvent(stor, eventType, action, ref, payload)

	for name, content := range workflowFiles {
		on, err := actions.ParseWorkflowOn(content)
		if err != nil {
			s.logger.Warn().Err(err).Str("file", name).Msg("skip workflow with invalid on: definition")
			continue
		}
		if !actions.WorkflowTriggersOn(on, ev) {
			continue
		}
		if s.actions.WorkflowFileDisabled(repoKey, name) {
			s.logger.Info().Str("file", name).Str("trigger", eventType).Msg("workflow disabled — not triggered")
			continue
		}

		meta := &actions.WorkflowEventMeta{
			EventName: eventType,
			Ref:       ref,
			Sha:       sha,
			Repo:      repoKey,
			Payload:   payload,
		}
		workflow, err := s.actions.SubmitTriggeredWorkflow(name, content, meta)
		if err != nil {
			// A matched workflow that can't start becomes a job-less
			// startup_failure run, visible on the runs API, not just a log.
			s.logger.Error().Err(err).Str("file", name).Msg("workflow failed at startup")
			s.createStartupFailureRun(name, content, meta)
			continue
		}

		s.logger.Info().
			Str("workflow_id", workflow.ID).
			Str("trigger", eventType).
			Str("action", action).
			Str("file", name).
			Msg("workflow triggered by event")
	}
}

// actionsEnabledForRepo applies the enterprise → organization → repository
// enablement chain: a narrower scope may restrict its parent, never re-enable
// what a broader scope disabled.
func (s *Server) actionsEnabledForRepo(repoKey string) bool {
	owner, name := splitRepoFull(repoKey)
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		return false
	}

	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	if repo.OwnerType == "Organization" {
		enterprise := s.store.EnterpriseSettings
		switch enterprise.ActionsEnabledOrganizations {
		case "none":
			return false
		case "selected":
			if !slices.Contains(enterprise.ActionsSelectedOrganizationIDs, repo.OwnerID) {
				return false
			}
		}
		if orgPolicy := s.store.LookupOrgActionsPermissionsLocked(owner); orgPolicy != nil {
			switch orgPolicy.EnabledRepositories {
			case "none":
				return false
			case "selected":
				if !slices.Contains(orgPolicy.SelectedRepositoryIDs, repo.ID) {
					return false
				}
			}
		}
	}
	if repoPolicy := s.store.RepoActionsPermissions[repo.FullName]; repoPolicy != nil {
		return repoPolicy.Enabled
	}
	return true
}

// workflowRunRefs separates the ref a workflow DEFINITION is read from and the
// ref/sha the run reports. They differ for a fork PR, whose branch does not
// exist in the base repository.
type workflowRunRefs struct {
	definitionRef string
	runRef        string
	runSha        string
}

// workflowRefsForEvent decides, per trigger type, which ref supplies the
// workflow definition and which ref/sha the run reports:
//
//   - push, release: the triggering ref, in the repo the event happened in.
//   - pull_request: definition from the base branch; run reports
//     refs/pull/<number>/merge with the synthetic merge SHA (head SHA fallback).
//   - pull_request_target: both use the base ref.
func workflowRefsForEvent(stor gitStorage.Storer, repoKey, eventType, ref string, payload map[string]interface{}) (workflowRunRefs, error) {
	if eventType == "pull_request" {
		if pr, _ := payload["pull_request"].(map[string]interface{}); pr != nil {
			baseRef := ""
			if base, _ := pr["base"].(map[string]interface{}); base != nil {
				if name, _ := base["ref"].(string); name != "" {
					baseRef = plumbing.NewBranchReferenceName(name).String()
				}
			}
			if baseRef == "" {
				return workflowRunRefs{}, fmt.Errorf("pull request payload names no base branch")
			}
			if sha := actions.ResolveRefSha(stor, baseRef); sha == actions.ZeroCommitSha {
				return workflowRunRefs{}, fmt.Errorf("base branch %s does not resolve to a commit", baseRef)
			}
			runSHA, _ := pr["merge_commit_sha"].(string)
			if runSHA == "" {
				head, _ := pr["head"].(map[string]interface{})
				if head != nil {
					headSha, _ := head["sha"].(string)
					runSHA = headSha
				}
			}
			if runSHA == "" {
				return workflowRunRefs{}, fmt.Errorf("pull request payload carries no merge or head sha")
			}
			runRef := ref
			if number := webhookPullRequestNumber(payload); number > 0 {
				runRef = fmt.Sprintf("refs/pull/%d/merge", number)
			}
			return workflowRunRefs{definitionRef: baseRef, runRef: runRef, runSha: runSHA}, nil
		}
	}
	if eventType == "pull_request_target" {
		definitionRef := defaultWorkflowDefinitionRef(stor, payload)
		if pr, _ := payload["pull_request"].(map[string]interface{}); pr != nil {
			if base, _ := pr["base"].(map[string]interface{}); base != nil {
				if name, _ := base["ref"].(string); name != "" {
					definitionRef = plumbing.NewBranchReferenceName(name).String()
				}
			}
		}
		sha := actions.ResolveRefSha(stor, definitionRef)
		if sha == actions.ZeroCommitSha {
			return workflowRunRefs{}, fmt.Errorf("base branch %s does not resolve to a commit", definitionRef)
		}
		return workflowRunRefs{definitionRef: definitionRef, runRef: definitionRef, runSha: sha}, nil
	}
	if eventType != "push" && eventType != "release" {
		definitionRef := defaultWorkflowDefinitionRef(stor, payload)
		sha := actions.ResolveRefSha(stor, definitionRef)
		if sha == actions.ZeroCommitSha {
			return workflowRunRefs{}, fmt.Errorf("default branch %s does not resolve to a commit", definitionRef)
		}
		runRef := ref
		runSHA := actions.ResolveRefSha(stor, runRef)
		if runSHA == actions.ZeroCommitSha {
			runRef = definitionRef
			runSHA = sha
		}
		return workflowRunRefs{definitionRef: definitionRef, runRef: runRef, runSha: runSHA}, nil
	}
	sha := actions.ResolveRefSha(stor, ref)
	if sha == actions.ZeroCommitSha {
		return workflowRunRefs{}, fmt.Errorf("ref %q does not resolve to a commit", ref)
	}
	return workflowRunRefs{definitionRef: ref, runRef: ref, runSha: sha}, nil
}

func webhookPullRequestNumber(payload map[string]interface{}) int {
	switch number := payload["number"].(type) {
	case int:
		return number
	case float64:
		return int(number)
	}
	if pull, _ := payload["pull_request"].(map[string]interface{}); pull != nil {
		switch number := pull["number"].(type) {
		case int:
			return number
		case float64:
			return int(number)
		}
	}
	return 0
}

func pushPayloadSkipsActions(payload map[string]interface{}) bool {
	head, _ := payload["head_commit"].(map[string]interface{})
	message, _ := head["message"].(string)
	message = strings.ToLower(message)
	for _, directive := range []string{"[skip ci]", "[ci skip]", "[no ci]", "[skip actions]", "[actions skip]"} {
		if strings.Contains(message, directive) {
			return true
		}
	}
	return false
}

func defaultWorkflowDefinitionRef(stor gitStorage.Storer, payload map[string]interface{}) string {
	if repository, _ := payload["repository"].(map[string]interface{}); repository != nil {
		if branch, _ := repository["default_branch"].(string); branch != "" {
			return plumbing.NewBranchReferenceName(branch).String()
		}
	}
	if head, err := stor.Reference(plumbing.HEAD); err == nil && head.Type() == plumbing.SymbolicReference {
		return head.Target().String()
	}
	return plumbing.NewBranchReferenceName("main").String()
}

// buildTriggerEvent assembles the filterable description of an event. Push
// changed files come from the before/after shas; for pull_request the filterable
// ref is the BASE branch and the diff spans base...head.
func (s *Server) buildTriggerEvent(stor gitStorage.Storer, eventType, action, ref string, payload map[string]interface{}) actions.TriggerEvent {
	ev := actions.TriggerEvent{Type: eventType, Action: action, Ref: ref}
	switch eventType {
	case "push":
		before, _ := payload["before"].(string)
		after, _ := payload["after"].(string)
		ev.ChangedFiles, ev.ChangedFilesKnown = actions.ChangedFilesBetween(stor, before, after)
	case "workflow_run":
		if run, _ := payload["workflow_run"].(map[string]interface{}); run != nil {
			ev.WorkflowName, _ = run["name"].(string)
		}
	case "pull_request", "pull_request_target":
		pr, _ := payload["pull_request"].(map[string]interface{})
		if pr != nil {
			if base, _ := pr["base"].(map[string]interface{}); base != nil {
				if baseRef, _ := base["ref"].(string); baseRef != "" {
					ev.Ref = "refs/heads/" + baseRef
					baseSha, _ := base["sha"].(string)
					if baseSha == "" {
						baseSha = store.ResolveBranchSha(stor, baseRef)
					}
					var headSha string
					if head, _ := pr["head"].(map[string]interface{}); head != nil {
						headSha, _ = head["sha"].(string)
						if headSha == "" {
							if headRef, _ := head["ref"].(string); headRef != "" {
								headSha = store.ResolveBranchSha(stor, headRef)
							}
						}
					}
					ev.ChangedFiles, ev.ChangedFilesKnown = actions.ChangedFilesBetween(stor, baseSha, headSha)
				}
			}
		}
	}
	return ev
}

// resolveGitHubRefInput resolves a `ref` input in any form GitHub accepts (full
// ref, branch, tag, raw SHA). The ref is normalized on a branch/tag hit;
// unresolved inputs return the zero SHA.
func resolveGitHubRefInput(stor gitStorage.Storer, ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ref, actions.ZeroCommitSha
	}
	candidates := []string{ref}
	if !strings.HasPrefix(ref, "refs/") {
		candidates = append(candidates,
			plumbing.NewBranchReferenceName(ref).String(),
			plumbing.NewTagReferenceName(ref).String(),
		)
	}
	for _, candidate := range candidates {
		if sha := actions.ResolveRefSha(stor, candidate); sha != actions.ZeroCommitSha {
			return candidate, sha
		}
	}
	if len(ref) == 40 {
		if _, err := hex.DecodeString(ref); err == nil {
			hash := plumbing.NewHash(ref)
			if _, err := stor.EncodedObject(plumbing.CommitObject, hash); err == nil {
				return ref, ref
			}
		}
	}
	return ref, actions.ZeroCommitSha
}

// createStartupFailureRun records a terminal, job-less run for a workflow that
// matched its trigger but could not start.
func (s *Server) createStartupFailureRun(fileName string, content []byte, meta *actions.WorkflowEventMeta) {
	name := workflowNameFromYAML(content)
	if name == "" {
		name = strings.TrimSuffix(strings.TrimSuffix(fileName, ".yml"), ".yaml")
	}
	wf := &store.Workflow{
		ID:           uuid.New().String(),
		Name:         name,
		RunID:        s.store.ReserveRunID(),
		Jobs:         map[string]*store.WorkflowJob{},
		Status:       store.WorkflowStatusCompleted,
		Result:       store.ResultStartupFailure,
		CreatedAt:    time.Now(),
		EventName:    meta.EventName,
		Ref:          meta.Ref,
		Sha:          meta.Sha,
		RepoFullName: meta.Repo,
		EventPayload: meta.Payload,
	}
	wf.WorkflowFileID, wf.WorkflowFilePath = s.actions.ResolveWorkflowFileForRun(wf)
	wf.RunNumber = s.store.ReserveWorkflowRunNumber(wf)
	// Once wf is published into s.store.Workflows another goroutine can mutate it
	// under store.mu, so the QueueEvent snapshots must run inside the same
	// critical section; doing it after Unlock is a data race.
	s.store.Mu.Lock()
	s.store.Workflows[wf.ID] = wf
	s.store.PersistWorkflowRecord(wf)
	s.actions.QueueEvent(actions.EvRunRequested, wf, nil)
	s.actions.QueueEvent(actions.EvRunCompleted, wf, nil)
	s.store.Mu.Unlock()
}

// workflowNameFromYAML extracts the workflow name, tolerating definitions too
// broken for ParseWorkflow.
func workflowNameFromYAML(content []byte) string {
	var raw struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(content, &raw); err != nil {
		return ""
	}
	return raw.Name
}
