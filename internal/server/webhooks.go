package bleephub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- GitHub's legacy X-Hub-Signature compatibility header requires HMAC-SHA1
	"crypto/sha256"
	"crypto/tls"
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
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"gopkg.in/yaml.v3"
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
	// webhookDeliveryWorkers bounds how many deliveries may be in flight at
	// once. A hook target that hangs costs one worker for the request timeout
	// instead of one goroutine per event for as long as events keep arriving.
	webhookDeliveryWorkers = 16
	webhookDialTimeout     = 5 * time.Second
	webhookRequestTimeout  = 10 * time.Second
	webhookResolveTimeout  = 2 * time.Second
)

// webhookAddrBlocked reports whether an IP address is one a webhook delivery
// must never reach. Anything not routable on the public internet is refused:
// the loopback interface, link-local space (which carries the cloud
// instance-metadata endpoint at 169.254.169.254), RFC1918 and IPv6
// unique-local space, carrier-grade NAT, multicast, and the unspecified and
// broadcast addresses.
func webhookAddrBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if nonPublicIP(ip) {
		return true
	}
	// An IPv6 form that tunnels an IPv4 address reaches whatever that address
	// reaches, so the embedded address is checked on its own terms.
	if v4 := tunnelledIPv4(ip); v4 != nil && nonPublicIP(v4) {
		return true
	}
	return false
}

// nonPublicIP is webhookAddrBlocked's per-address-family core.
func nonPublicIP(ip net.IP) bool {
	// IsGlobalUnicast is false for the unspecified address, loopback,
	// multicast, link-local unicast and the IPv4 broadcast address.
	if !ip.IsGlobalUnicast() {
		return true
	}
	// IsPrivate covers 10/8, 172.16/12, 192.168/16 and fc00::/7 (which
	// contains the fd00::/8 unique-local range).
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

// parseWebhookTargetURL checks the parts of a target URL that need no name
// resolution — the scheme, the presence of a host, and a literal IP — and
// returns the parsed URL. This is the check the delivery path can afford to
// repeat on every attempt.
func parseWebhookTargetURL(raw string, allowPrivate bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("webhook URL %q is not a valid URL: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("webhook URL scheme %q is not deliverable; use http or https", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("webhook URL %q names no host", raw)
	}
	if allowPrivate {
		return u, nil
	}
	if ip := net.ParseIP(host); ip != nil && webhookAddrBlocked(ip) {
		return nil, fmt.Errorf("webhook target %s is not a public address", ip)
	}
	return u, nil
}

// validateWebhookTargetURL is the configuration-time gate: the shape check
// plus one name resolution, so a hostname pointing at private space is
// refused when the hook is stored rather than when it first fires.
//
// A name that does not resolve is stored. It admits nothing — the address
// actually dialed is checked again at delivery time, which is also the only
// point where a rebinding answer can be caught.
func validateWebhookTargetURL(raw string, allowPrivate bool) error {
	u, err := parseWebhookTargetURL(raw, allowPrivate)
	if err != nil {
		return err
	}
	if allowPrivate {
		return nil
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

// webhookDialControl is the delivery-time address gate. It runs after the
// resolver has answered and before the socket connects, with the address that
// is about to be dialed — the only point where the address checked and the
// address reached cannot differ.
func webhookDialControl(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
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

// newWebhookClient builds the HTTP client webhook deliveries use.
func newWebhookClient(allowPrivate, insecureTLS bool) *http.Client {
	return newWebhookClientWithTimeout(allowPrivate, insecureTLS, webhookRequestTimeout)
}

func newWebhookClientWithTimeout(allowPrivate, insecureTLS bool, timeout time.Duration) *http.Client {
	transport := newAddressCheckedHTTPTransport(allowPrivate, insecureTLS)
	return &http.Client{
		Timeout:   timeout,
		Transport: otelhttp.NewTransport(transport),
		// GitHub does not follow redirects on webhook delivery. Returning the
		// 3xx as the outcome keeps a redirect from reaching a second
		// destination the address gate never saw.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// newAddressCheckedHTTPTransport is the shared server-side request boundary
// for URL fetchers. Configuration-time DNS checks are useful feedback, but
// only Dialer.Control sees the address the kernel is actually about to reach;
// keeping the transport construction shared prevents webhook and source-import
// SSRF policy from drifting apart again.
func newAddressCheckedHTTPTransport(allowPrivate, insecureTLS bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   webhookDialTimeout,
		KeepAlive: 30 * time.Second,
		Control:   webhookDialControl(allowPrivate),
	}
	transport := &http.Transport{
		// No proxy: a proxied request dials the proxy, so the address gate
		// would inspect the proxy instead of the target it is there to check.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if insecureTLS {
		// #nosec G402 -- hook config insecure_ssl=1 explicitly disables
		// verification on GitHub too; redirects and private-address dials remain blocked.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return transport
}

// webhookDeliveryClient returns the shared client for a hook's TLS setting.
func (s *Server) webhookDeliveryClient(insecureTLS bool) *http.Client {
	s.webhookClientsOnce.Do(func() {
		s.webhookClients = [2]*http.Client{
			newWebhookClient(s.allowPrivateOutboundTargets, false),
			newWebhookClient(s.allowPrivateOutboundTargets, true),
		}
	})
	if insecureTLS {
		return s.webhookClients[1]
	}
	return s.webhookClients[0]
}

// webhookDispatcher runs webhook deliveries on a fixed pool of workers while
// keeping the deliveries of any one hook in the order they were queued: each
// hook has its own FIFO queue, and a queue is drained by one worker at a time.
type webhookDispatcher struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queues map[string]*webhookQueue
	ready  []*webhookQueue
}

type webhookQueue struct {
	key     string
	pending []func()
	// queued marks the queue as either waiting on the ready list or being
	// drained; it is what stops two workers from reordering one hook's events.
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

// webhookQueueKey identifies the delivery order domain a hook belongs to.
// Hook ids are unique across repository, organization and app hooks, but the
// owner is included so a reloaded hook cannot collide with a live one.
func webhookQueueKey(h *Webhook) string {
	global := ""
	if h.Global {
		global = "global"
	}
	return h.RepoKey + "\x00" + h.OrgLogin + "\x00" + h.MarketplaceSlug + "\x00" + global + "\x00" + strconv.Itoa(h.ID)
}

// SnapshotHook copies the configuration a delivery reads, under the store
// lock. Hook edits mutate the stored *Webhook in place, so a delivery holding
// the shared pointer races every PATCH of the hook it is delivering — and a
// delivery is addressed and signed by the configuration as of the moment it
// was queued, not by whatever the hook becomes mid-flight.
func (st *Store) SnapshotHook(h *Webhook) *Webhook {
	if h == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	snapshot := cloneWebhook(h)
	snapshot.LastResponse = nil
	return snapshot
}

// appWebhookPseudoHook is the Webhook view of a GitHub App's single webhook
// configuration. The negative id is what marks an app-level delivery
// throughout the delivery path.
func appWebhookPseudoHook(app *App) *Webhook {
	return &Webhook{
		ID:     -app.ID,
		URL:    app.WebhookURL,
		Secret: app.WebhookSecret,
		Events: app.WebhookEvents,
		Active: app.WebhookActive,
	}
}

func appWebhookQueueKey(app *App) string {
	return "app\x00" + strconv.Itoa(app.ID)
}

// enqueueWebhookJob schedules work on the delivery pool, ordered behind
// anything already queued for the same key.
func (s *Server) enqueueWebhookJob(key string, job func()) {
	s.webhookDispatch().enqueue(key, job)
}

// enqueueWebhookDelivery is the asynchronous form of deliverWebhook. Every
// event fan-out goes through here rather than a bare `go`, so an event storm
// cannot spawn a goroutine per hook per event.
func (s *Server) enqueueWebhookDelivery(hook *Webhook, event, action string, payloadBytes []byte) {
	queued := s.store.SnapshotHook(hook)
	s.enqueueWebhookJob(webhookQueueKey(queued), func() {
		s.deliverWebhook(queued, event, action, payloadBytes)
	})
}

// redeliverWebhook re-runs one recorded delivery and stores the new attempt.
func (s *Server) redeliverWebhook(hook *Webhook, event, action, guid string, payloadBytes []byte) {
	hook = s.store.SnapshotHook(hook)
	delivery := s.doDeliverAttempt(hook, event, action, guid, payloadBytes, true)
	s.store.AddDelivery(delivery)
	s.recordHookLastResponse(hook, delivery)
}

// recordHookLastResponse writes the attempt outcome to whichever hook table
// owns the hook. Repository and organization hooks both have a last_response,
// and each redelivery path used to update only its own half.
func (s *Server) recordHookLastResponse(hook *Webhook, delivery *WebhookDelivery) {
	switch {
	case hook.RepoKey != "":
		s.store.SetHookLastResponse(hook.RepoKey, hook.ID, deliveryLastResponse(delivery))
	case hook.OrgLogin != "":
		s.store.SetOrgHookLastResponse(hook.OrgLogin, hook.ID, deliveryLastResponse(delivery))
	case hook.Global:
		s.store.mu.Lock()
		for _, stored := range s.store.EnterpriseSettings.GHESGlobalHooks {
			if stored.ID == hook.ID {
				stored.LastResponse = deliveryLastResponse(delivery)
				stored.UpdatedAt = s.store.currentTime()
				break
			}
		}
		s.store.persistEnterpriseSettings()
		s.store.mu.Unlock()
	}
}

// emitWebhookEvent dispatches an event to matching webhooks (non-blocking).
// Org-owned repos additionally fan the event out to the owner org's hooks —
// real GitHub delivers every repo event on an org repository to matching
// organization webhooks too.
func (s *Server) emitWebhookEvent(repoKey, eventType, action string, payload interface{}) {
	hooks := s.store.ListHooks(repoKey)
	if ownerLogin, _, found := strings.Cut(repoKey, "/"); found {
		if s.store.GetOrg(ownerLogin) != nil {
			hooks = append(hooks, s.store.ListOrgHooks(ownerLogin)...)
		}
	}

	// Org-owned repos carry a top-level `organization` object on event
	// payloads (real GitHub adds it for every event on an org repo).
	// Attached centrally: every repo event funnels through here, and the
	// store lookup needs server access the payload builders don't have.
	if m, ok := payload.(map[string]interface{}); ok {
		if _, has := m["organization"]; !has {
			ownerLogin, _, found := strings.Cut(repoKey, "/")
			if found {
				if org := s.store.GetOrg(ownerLogin); org != nil {
					m["organization"] = orgWebhookPayload(org)
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

// triggerWorkflowsForWebhookEvent makes webhook and Actions event production
// one application concept. A mutator cannot implement an event for hooks while
// silently forgetting that the same event triggers workflows.
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
				owner, name, _ := splitRepoFullName(repo.FullName)
				stor := s.store.GetGitStorage(owner, name)
				if stor != nil {
					if normalized, sha := resolveGitHubRefInput(stor, deploymentRef); sha != zeroCommitSha {
						ref = normalized
					}
				}
			}
		}
	}
	s.triggerWorkflowsForEvent(repoKey, eventType, action, ref, payload)
}

func hookMatchesEvent(hook *Webhook, eventType string) bool {
	for _, e := range hook.Events {
		if e == eventType || e == "*" {
			return true
		}
	}
	return false
}

// deliverWebhook sends an HTTP POST with retries (3 attempts, exponential backoff).
func (s *Server) deliverWebhook(hook *Webhook, event, action string, payloadBytes []byte) {
	hook = s.store.SnapshotHook(hook)
	guid := uuid.New().String()
	backoffs := []time.Duration{0, 1 * time.Second, 5 * time.Second}
	if _, err := parseWebhookTargetURL(hook.URL, s.allowPrivateOutboundTargets); err != nil {
		// A refused target does not become deliverable by waiting: record the
		// refusal once instead of retrying it.
		backoffs = backoffs[:1]
	}

	for attempt, backoff := range backoffs {
		if attempt > 0 {
			time.Sleep(backoff)
		}

		delivery := s.doDeliverAttempt(hook, event, action, guid, payloadBytes, attempt > 0)
		s.store.AddDelivery(delivery)
		s.recordHookLastResponse(hook, delivery)

		if delivery.StatusCode >= 200 && delivery.StatusCode < 300 {
			return
		}
	}
}

// deliveryLastResponse maps a delivery attempt to the hook.last_response shape.
func deliveryLastResponse(d *WebhookDelivery) *HookLastResponse {
	msg := http.StatusText(d.StatusCode)
	if d.StatusCode >= 200 && d.StatusCode < 300 {
		msg = "OK"
	} else if d.StatusCode == 0 {
		msg = "failed to connect"
	}
	return &HookLastResponse{
		Code:    d.StatusCode,
		Status:  deliveryStatus(d.StatusCode),
		Message: msg,
	}
}

// doDeliverAttempt performs a single webhook delivery attempt.
func (s *Server) doDeliverAttempt(hook *Webhook, event, action, guid string, payloadBytes []byte, redelivery bool) *WebhookDelivery {
	hook = s.store.SnapshotHook(hook)
	start := time.Now()

	// content_type=form (GitHub's default) sends the JSON payload as the
	// value of a `payload` form field with an x-www-form-urlencoded body,
	// and signs THAT body — not the raw JSON. content_type=json sends the
	// JSON verbatim. The stored hook.ContentType picks which.
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
	// Installation-target headers identify the resource that owns the hook.
	// App-bound hooks (HookID < 0) target the GitHub App ("integration");
	// org hooks target the organization's id; repository hooks the repo's.
	switch {
	case hook.MarketplaceSlug != "":
		// GitHub Marketplace webhooks are listing-scoped and do not advertise a
		// repository, organization, or GitHub App installation target.
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
		parts := splitRepoKeyParts(hook.RepoKey)
		if parts[1] != "" {
			if repo := s.store.GetRepo(parts[0], parts[1]); repo != nil {
				reqHeaders["X-GitHub-Hook-Installation-Target-ID"] = strconv.Itoa(repo.ID)
			}
		}
	}

	// undelivered records an attempt that never reached the network.
	undelivered := func(err error) *WebhookDelivery {
		return &WebhookDelivery{
			HookID:      hook.ID,
			TargetURL:   hook.URL,
			GUID:        guid,
			Event:       event,
			Action:      action,
			StatusCode:  0,
			Duration:    time.Since(start).Seconds(),
			Request:     &DeliveryRequest{Headers: reqHeaders, Payload: json.RawMessage(payloadBytes)},
			Response:    &DeliveryResponse{StatusCode: 0, Body: err.Error()},
			Redelivery:  redelivery,
			DeliveredAt: time.Now(),
		}
	}

	// Every delivery path funnels through here, so this is where the stored
	// target is re-checked — a hook configured before a rule tightened, or
	// written straight into the store, is refused just the same.
	if _, err := parseWebhookTargetURL(hook.URL, s.allowPrivateOutboundTargets); err != nil {
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

	resp, err := s.webhookDeliveryClient(hook.InsecureSSL == "1").Do(httpReq)
	elapsed := time.Since(start).Seconds()

	delivery := &WebhookDelivery{
		HookID:      hook.ID,
		TargetURL:   hook.URL,
		GUID:        guid,
		Event:       event,
		Action:      action,
		Redelivery:  redelivery,
		DeliveredAt: time.Now(),
		Duration:    elapsed,
		Request:     &DeliveryRequest{Headers: reqHeaders, Payload: json.RawMessage(payloadBytes)},
	}

	if err != nil {
		delivery.StatusCode = 0
		delivery.Response = &DeliveryResponse{StatusCode: 0, Body: err.Error()}
		return delivery
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	respHeaders := make(map[string]string)
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}

	delivery.StatusCode = resp.StatusCode
	delivery.Response = &DeliveryResponse{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       string(respBody),
	}

	return delivery
}

// triggerWorkflowsForEvent triggers matching workflows from git storage
// for a concrete event occurrence. action carries the activity type
// ("opened", "synchronize", repository_dispatch's event_type, ...);
// payload is the webhook payload the event emitted — it becomes the
// run's github.event context and feeds the trigger filters (push
// before/after shas, PR base branch).
func (s *Server) triggerWorkflowsForEvent(repoKey, eventType, action, ref string, payload map[string]interface{}) {
	parts := splitRepoKeyParts(repoKey)
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

	workflowFiles := listWorkflowFilesAtRef(stor, refs.definitionRef)
	if len(workflowFiles) == 0 {
		return
	}

	ref = refs.runRef
	ev := s.buildTriggerEvent(stor, eventType, action, ref, payload)

	for name, content := range workflowFiles {
		on, err := ParseWorkflowOn(content)
		if err != nil {
			s.logger.Warn().Err(err).Str("file", name).Msg("skip workflow with invalid on: definition")
			continue
		}
		if !workflowTriggersOn(on, ev) {
			continue
		}
		if s.workflowFileDisabled(repoKey, name) {
			s.logger.Info().Str("file", name).Str("trigger", eventType).Msg("workflow disabled — not triggered")
			continue
		}

		meta := &WorkflowEventMeta{
			EventName: eventType,
			Ref:       ref,
			Sha:       sha,
			Repo:      repoKey,
			Payload:   payload,
		}
		workflow, err := s.submitTriggeredWorkflow(name, content, meta)
		if err != nil {
			// Real GitHub creates a run with conclusion startup_failure
			// (no jobs) when a matched workflow can't start — the
			// failure must be visible on the runs API, not just a log.
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
// enablement chain. Each narrower scope may further restrict its parent; none
// can re-enable Actions when a broader scope disabled it.
func (s *Server) actionsEnabledForRepo(repoKey string) bool {
	owner, name := splitRepoFull(repoKey)
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		return false
	}

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
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
		if orgPolicy := s.store.lookupOrgActionsPermissionsLocked(owner); orgPolicy != nil {
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

// submitTriggeredWorkflow parses, expands, and submits one workflow file
// for an event. Event metadata must travel INTO submitWorkflow: it
// resolves the originating workflow file from RepoFullName at submit
// time (the run's workflow_id), and the workflow becomes visible to
// other goroutines the moment it is stored — patching fields afterwards
// would both mis-derive the file id and race those readers.
func (s *Server) submitTriggeredWorkflow(fileName string, content []byte, meta *WorkflowEventMeta) (*Workflow, error) {
	wfDef, err := ParseWorkflow(content)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", fileName, err)
	}

	expandedDef := expandMatrixJobs(wfDef)

	if expandedDef.Env == nil {
		expandedDef.Env = make(map[string]string)
	}
	expandedDef.Env["__defaultImage"] = ""

	serverURL := fmt.Sprintf("http://%s", s.addr)
	expandedDef.Env["__serverURL"] = serverURL

	return s.submitWorkflow(context.Background(), serverURL, expandedDef, "", meta)
}

// workflowRunRefs separates the two refs an event carries, which used to be
// one: the ref a workflow DEFINITION is read from, and the ref/sha the run
// reports. They differ for a pull request opened from a fork, where the fork's
// branch does not exist in the base repository at all.
type workflowRunRefs struct {
	definitionRef string
	runRef        string
	runSha        string
}

// workflowRefsForEvent decides, per trigger type, which ref supplies the
// workflow definition and which ref/sha the run reports.
//
//   - push and release:
//     the triggering ref, in the repository the event happened in. Reading
//     HEAD instead ran whatever the default branch happened to say, which is
//     not the definition the event was raised against.
//   - pull_request: the definition comes from the base branch and the run
//     reports refs/pull/<number>/merge with the event's synthetic merge SHA
//     (falling back to the head SHA while no merge candidate can be created).
//   - pull_request_target: both definition and run identity use the base ref.
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
			if sha := resolveRefSha(stor, baseRef); sha == zeroCommitSha {
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
		sha := resolveRefSha(stor, definitionRef)
		if sha == zeroCommitSha {
			return workflowRunRefs{}, fmt.Errorf("base branch %s does not resolve to a commit", definitionRef)
		}
		return workflowRunRefs{definitionRef: definitionRef, runRef: definitionRef, runSha: sha}, nil
	}
	if eventType != "push" && eventType != "release" {
		definitionRef := defaultWorkflowDefinitionRef(stor, payload)
		sha := resolveRefSha(stor, definitionRef)
		if sha == zeroCommitSha {
			return workflowRunRefs{}, fmt.Errorf("default branch %s does not resolve to a commit", definitionRef)
		}
		runRef := ref
		runSHA := resolveRefSha(stor, runRef)
		if runSHA == zeroCommitSha {
			runRef = definitionRef
			runSHA = sha
		}
		return workflowRunRefs{definitionRef: definitionRef, runRef: runRef, runSha: runSHA}, nil
	}
	sha := resolveRefSha(stor, ref)
	if sha == zeroCommitSha {
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

// pullRequestIsFromFork reports whether the event payload's pull request has
// its head in a repository other than repoKey.
func pullRequestIsFromFork(payload map[string]interface{}, repoKey string) bool {
	pr, _ := payload["pull_request"].(map[string]interface{})
	if pr == nil || repoKey == "" {
		return false
	}
	head, _ := pr["head"].(map[string]interface{})
	if head == nil {
		return false
	}
	headRepo, _ := head["repo"].(map[string]interface{})
	if headRepo == nil {
		return false
	}
	headFullName, _ := headRepo["full_name"].(string)
	return headFullName != "" && !strings.EqualFold(headFullName, repoKey)
}

// buildTriggerEvent assembles the filterable description of an event
// occurrence. For pushes the changed files come from the payload's
// before/after shas; for pull_request events the filterable ref is the
// BASE branch and the diff spans base...head.
func (s *Server) buildTriggerEvent(stor gitStorage.Storer, eventType, action, ref string, payload map[string]interface{}) triggerEvent {
	ev := triggerEvent{Type: eventType, Action: action, Ref: ref}
	switch eventType {
	case "push":
		before, _ := payload["before"].(string)
		after, _ := payload["after"].(string)
		ev.ChangedFiles, ev.ChangedFilesKnown = changedFilesBetween(stor, before, after)
	case "pull_request", "pull_request_target":
		pr, _ := payload["pull_request"].(map[string]interface{})
		if pr != nil {
			if base, _ := pr["base"].(map[string]interface{}); base != nil {
				if baseRef, _ := base["ref"].(string); baseRef != "" {
					ev.Ref = "refs/heads/" + baseRef
					baseSha, _ := base["sha"].(string)
					if baseSha == "" {
						baseSha = resolveBranchSha(stor, baseRef)
					}
					var headSha string
					if head, _ := pr["head"].(map[string]interface{}); head != nil {
						headSha, _ = head["sha"].(string)
						if headSha == "" {
							if headRef, _ := head["ref"].(string); headRef != "" {
								headSha = resolveBranchSha(stor, headRef)
							}
						}
					}
					ev.ChangedFiles, ev.ChangedFilesKnown = changedFilesBetween(stor, baseSha, headSha)
				}
			}
		}
	}
	return ev
}

// workflowFileDisabled reports whether the registered workflow file for
// (repo, filename) was manually disabled — disabled workflows never
// trigger, matching real GitHub.
func (s *Server) workflowFileDisabled(repoKey, filename string) bool {
	path := ".github/workflows/" + filename
	for _, f := range s.store.ListWorkflowFiles(repoKey) {
		if f.Path == path {
			return strings.HasPrefix(f.State, "disabled")
		}
	}
	return false
}

// zeroCommitSha is the all-zero sha resolveRefSha returns when a ref names no
// commit.
const zeroCommitSha = "0000000000000000000000000000000000000000"

// resolveRefSha resolves the commit sha the triggering ref points at in git
// storage. Empty ref means HEAD. Non-empty refs must resolve exactly; event
// triggers must not silently substitute a different commit.
func resolveRefSha(stor gitStorage.Storer, ref string) string {
	resolve := func(name plumbing.ReferenceName) (plumbing.Hash, bool) {
		r, err := stor.Reference(name)
		if err != nil {
			return plumbing.Hash{}, false
		}
		if r.Type() == plumbing.SymbolicReference {
			target, err := stor.Reference(r.Target())
			if err != nil {
				return plumbing.Hash{}, false
			}
			return target.Hash(), true
		}
		return r.Hash(), true
	}
	if ref != "" {
		if h, ok := resolve(plumbing.ReferenceName(ref)); ok && !h.IsZero() {
			return h.String()
		}
		return zeroCommitSha
	}
	if h, ok := resolve(plumbing.HEAD); ok && !h.IsZero() {
		return h.String()
	}
	return zeroCommitSha
}

// resolveGitHubRefInput resolves a GitHub API `ref` input. Public endpoints
// accept the same forms real GitHub accepts: full refs, branch names, tag
// names, and raw commit SHAs. The returned ref is normalized when a branch or
// tag name resolves; unresolved inputs stay fail-loud through the zero SHA.
func resolveGitHubRefInput(stor gitStorage.Storer, ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ref, zeroCommitSha
	}
	candidates := []string{ref}
	if !strings.HasPrefix(ref, "refs/") {
		candidates = append(candidates,
			plumbing.NewBranchReferenceName(ref).String(),
			plumbing.NewTagReferenceName(ref).String(),
		)
	}
	for _, candidate := range candidates {
		if sha := resolveRefSha(stor, candidate); sha != zeroCommitSha {
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
	return ref, zeroCommitSha
}

func splitRepoKeyParts(repoKey string) [2]string {
	for i, c := range repoKey {
		if c == '/' {
			return [2]string{repoKey[:i], repoKey[i+1:]}
		}
	}
	return [2]string{repoKey, ""}
}

// listWorkflowFilesAtRef reads .github/workflows as of ref. An empty ref means
// HEAD.
func listWorkflowFilesAtRef(stor gitStorage.Storer, ref string) map[string][]byte {
	sha := resolveRefSha(stor, ref)
	if sha == zeroCommitSha {
		return nil
	}
	commitHash := plumbing.NewHash(sha)

	commit, err := object.GetCommit(stor, commitHash)
	if err != nil {
		return nil
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil
	}

	ghEntry, err := tree.FindEntry(".github")
	if err != nil {
		return nil
	}
	ghTree, err := object.GetTree(stor, ghEntry.Hash)
	if err != nil {
		return nil
	}
	wfEntry, err := ghTree.FindEntry("workflows")
	if err != nil {
		return nil
	}
	wfTree, err := object.GetTree(stor, wfEntry.Hash)
	if err != nil {
		return nil
	}

	result := make(map[string][]byte)
	for _, entry := range wfTree.Entries {
		if !entry.Mode.IsFile() {
			continue
		}
		if !strings.HasSuffix(entry.Name, ".yml") && !strings.HasSuffix(entry.Name, ".yaml") {
			continue
		}
		blob, err := object.GetBlob(stor, entry.Hash)
		if err != nil {
			continue
		}
		reader, err := blob.Reader()
		if err != nil {
			continue
		}
		content, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			continue
		}
		result[entry.Name] = content
	}
	return result
}

// createStartupFailureRun records a terminal, job-less run for a
// workflow that matched its trigger but could not start.
func (s *Server) createStartupFailureRun(fileName string, content []byte, meta *WorkflowEventMeta) {
	name := workflowNameFromYAML(content)
	if name == "" {
		name = strings.TrimSuffix(strings.TrimSuffix(fileName, ".yml"), ".yaml")
	}
	wf := &Workflow{
		ID:           uuid.New().String(),
		Name:         name,
		RunID:        s.store.ReserveRunID(),
		Jobs:         map[string]*WorkflowJob{},
		Status:       WorkflowStatusCompleted,
		Result:       ResultStartupFailure,
		CreatedAt:    time.Now(),
		EventName:    meta.EventName,
		Ref:          meta.Ref,
		Sha:          meta.Sha,
		RepoFullName: meta.Repo,
		EventPayload: meta.Payload,
	}
	wf.WorkflowFileID, wf.WorkflowFilePath = s.resolveWorkflowFileForRun(wf)
	wf.RunNumber = s.store.ReserveWorkflowRunNumber(wf)
	s.store.mu.Lock()
	s.store.Workflows[wf.ID] = wf
	s.store.persistWorkflowRecord(wf)
	s.store.mu.Unlock()
	s.queueActionsEvent(evRunRequested, wf, nil)
	s.queueActionsEvent(evRunCompleted, wf, nil)
}

// workflowNameFromYAML extracts just the workflow's name, tolerating
// definitions too broken for ParseWorkflow.
func workflowNameFromYAML(content []byte) string {
	var raw struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(content, &raw); err != nil {
		return ""
	}
	return raw.Name
}
