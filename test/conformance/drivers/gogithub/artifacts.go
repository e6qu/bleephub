// Artifact production and retrieval.
//
// An Actions artifact cannot be created through the public Representational
// State Transfer surface: GitHub has no artifact-upload route there. The only
// producer is a runner executing a job, which uploads over the Actions results
// protocol with the per-job runtime token the broker hands it in the job
// message. So to assert anything about artifacts at all, this file speaks that
// protocol — it registers a just-in-time runner through the documented REST
// route, exchanges the runner's own key for a session, leases the dispatched
// job, and uploads with the runtime token that lease produced. Nothing here
// mints a credential: every token comes from the server, through the same
// exchange the official runner performs.
//
// Once the artifact exists, the assertions that matter are made by the
// software development kit's typed methods — list, metadata, download — and
// the downloaded bytes are unzipped and compared with what was uploaded.
package main

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	github "github.com/google/go-github/v88/github"
)

// artifactWorkflow is dispatch-only on purpose: a `push` trigger would queue a
// second run when the file is committed, and the just-in-time runner below is
// ephemeral — it may lease exactly one job.
const artifactWorkflow = `name: artifacts
on:
  workflow_dispatch:
jobs:
  package:
    runs-on: ubuntu-latest
    steps:
      - run: echo build the artifact
`

const (
	artifactName      = "conformance-artifact"
	artifactEntryName = "conformance.txt"
	artifactEntryBody = "produced by a bleephub conformance job\n"
)

// artifactOperations are the two operations this group records. They are named
// once so a fixture failure can skip exactly them.
var artifactOperations = []string{"actions.uploadArtifact", "actions.downloadArtifact"}

// runActionsArtifacts publishes a workflow, leases its job as a runner,
// uploads an artifact over the results protocol, completes the job, and then
// reads the artifact back through the software development kit.
func runActionsArtifacts(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "actions"

	sc := newScratch(client, set.owner, "conformance-actions-artifacts")
	if !sc.ok() {
		skipAll(rec, domain, "POST /user/repos", "the artifact repository fixture could not be provisioned",
			artifactOperations...)
		return
	}
	if _, err := commitFile(client, sc, ".github/workflows/artifacts.yml",
		"add the artifact workflow", artifactWorkflow); err != nil {
		skipAll(rec, domain, "PUT /repos/{owner}/{repo}/contents/.github/workflows/artifacts.yml",
			"the artifact workflow could not be committed: "+truncate(err.Error()), artifactOperations...)
		return
	}

	runner, err := newJITRunner(client, sc)
	if err != nil {
		skipAll(rec, domain, "POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig",
			"a runner could not be registered: "+truncate(err.Error()), artifactOperations...)
		return
	}
	defer runner.close()

	if _, _, err := client.Actions.CreateWorkflowDispatchEventByFileName(ctx, sc.owner, sc.repo,
		"artifacts.yml", github.CreateWorkflowDispatchEventRequest{Ref: sc.branch}); err != nil {
		skipAll(rec, domain, "POST /repos/{owner}/{repo}/actions/workflows/{workflow_id}/dispatches",
			"the artifact workflow could not be dispatched: "+truncate(err.Error()), artifactOperations...)
		return
	}

	var runID int64
	if err := pollUntil("the dispatched artifact run appears", 30*time.Second, func() (bool, error) {
		runs, _, err := client.Actions.ListWorkflowRunsByFileName(ctx, sc.owner, sc.repo, "artifacts.yml", nil)
		if err != nil {
			return false, err
		}
		if len(runs.WorkflowRuns) == 0 {
			return false, nil
		}
		runID = runs.WorkflowRuns[0].GetID()
		return runID != 0, nil
	}); err != nil {
		skipAll(rec, domain, "GET /repos/{owner}/{repo}/actions/workflows/{workflow_id}/runs",
			"the dispatched run never appeared: "+truncate(err.Error()), artifactOperations...)
		return
	}

	job, err := runner.leaseJob()
	if err != nil {
		skipAll(rec, domain, "GET /_apis/v1/Message/{poolId}",
			"the dispatched job was never handed to the runner: "+truncate(err.Error()), artifactOperations...)
		return
	}

	payload, err := artifactZip()
	if err != nil {
		skipAll(rec, domain, "PUT /_apis/v1/artifacts/{artifact_id}/upload",
			"the artifact archive could not be built: "+truncate(err.Error()), artifactOperations...)
		return
	}
	backendID := strconv.FormatInt(runID, 10)

	var uploadedID int64
	rec.check(domain, "actions.uploadArtifact", "PUT /_apis/v1/artifacts/{artifact_id}/upload", func() error {
		uploadURL, err := job.createArtifact(backendID, artifactName)
		if err != nil {
			return err
		}
		if uploadURL == "" {
			return deviate("a signed upload URL", "empty",
				"CreateArtifact answered without the upload URL the toolkit puts the bytes to")
		}
		if err := job.uploadArtifact(uploadURL, payload); err != nil {
			return err
		}
		id, err := job.finalizeArtifact(backendID, artifactName, int64(len(payload)))
		if err != nil {
			return err
		}
		if id == 0 {
			return deviate("a non-zero artifact id", "0", "FinalizeArtifact answered without an artifact id")
		}
		uploadedID = id
		// The run-scoped listing the toolkit reads back is the same view the
		// client uses to find what it just wrote, so a finalized artifact that
		// does not appear there is an upload that did not take effect.
		names, err := job.listArtifacts(backendID)
		if err != nil {
			return err
		}
		for _, name := range names {
			if name == artifactName {
				return nil
			}
		}
		return deviate(artifactName, strings.Join(names, ","),
			"the finalized artifact is absent from the run's artifact listing")
	})

	// The job is reported complete over the same protocol, so the run reaches a
	// terminal state exactly as it would with the official runner.
	if err := job.complete(); err != nil {
		rec.check(domain, "actions.downloadArtifact",
			"GET /repos/{owner}/{repo}/actions/artifacts/{id}/{archive_format}", func() error {
				return deviate("the leased job completes", truncate(err.Error()),
					"the runner could not report job completion: %v", err)
			})
		return
	}

	rec.check(domain, "actions.downloadArtifact",
		"GET /repos/{owner}/{repo}/actions/artifacts/{id}/{archive_format}", func() error {
			if uploadedID == 0 {
				return deviate("an uploaded artifact", "none", "no artifact was produced to download")
			}
			if err := pollUntil("the run that produced the artifact completes", 30*time.Second,
				func() (bool, error) {
					run, _, err := client.Actions.GetWorkflowRunByID(ctx, sc.owner, sc.repo, runID)
					if err != nil {
						return false, err
					}
					return run.GetStatus() == "completed", nil
				}); err != nil {
				return err
			}

			artifacts, _, err := client.Actions.ListWorkflowRunArtifacts(ctx, sc.owner, sc.repo, runID, nil)
			if err != nil {
				return err
			}
			var found *github.Artifact
			for _, art := range artifacts.Artifacts {
				if art.GetName() == artifactName {
					found = art
					break
				}
			}
			if found == nil {
				return deviate(artifactName, fmt.Sprintf("%d artifacts", artifacts.GetTotalCount()),
					"the run's artifact listing does not contain the artifact its job uploaded")
			}
			if found.GetSizeInBytes() != int64(len(payload)) {
				return deviate(fmt.Sprintf("%d", len(payload)), fmt.Sprintf("%d", found.GetSizeInBytes()),
					"the artifact's reported size is not the number of bytes uploaded")
			}
			if found.GetWorkflowRun().GetID() != runID {
				return deviate(fmt.Sprintf("%d", runID), fmt.Sprintf("%d", found.GetWorkflowRun().GetID()),
					"the artifact is not attributed to the run that produced it")
			}
			digest := sha256.Sum256(payload)
			if want := "sha256:" + fmt.Sprintf("%x", digest); found.GetDigest() != want {
				return deviate(want, found.GetDigest(), "the artifact digest is not the digest of its bytes")
			}

			metadata, _, err := client.Actions.GetArtifact(ctx, sc.owner, sc.repo, found.GetID())
			if err != nil {
				return err
			}
			if metadata.GetID() != found.GetID() {
				return deviate(fmt.Sprintf("%d", found.GetID()), fmt.Sprintf("%d", metadata.GetID()),
					"fetching the artifact by id returned a different artifact")
			}

			location, _, err := client.Actions.DownloadArtifact(ctx, sc.owner, sc.repo, found.GetID(), 1)
			if err != nil {
				return err
			}
			if location == nil || location.String() == "" {
				return deviate("a redirect to the archive", "no Location",
					"the archive endpoint answered without the redirect clients follow")
			}
			body, err := fetchArtifactArchive(location)
			if err != nil {
				return err
			}
			if !bytes.Equal(body, payload) {
				return deviate(fmt.Sprintf("the %d uploaded bytes", len(payload)),
					fmt.Sprintf("%d bytes", len(body)), "the downloaded archive is not the archive that was uploaded")
			}
			entry, err := artifactZipEntry(body)
			if err != nil {
				return err
			}
			if entry != artifactEntryBody {
				return deviate(artifactEntryBody, entry, "the archive's entry does not carry the bytes the job wrote")
			}
			return nil
		})
}

// artifactZip builds the archive an upload puts, in the form the artifact
// toolkit uploads: a zip carrying the job's files.
func artifactZip() ([]byte, error) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	entry, err := archive.Create(artifactEntryName)
	if err != nil {
		return nil, err
	}
	if _, err := entry.Write([]byte(artifactEntryBody)); err != nil {
		return nil, err
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// artifactZipEntry reads the single entry back out of a downloaded archive, so
// the assertion is on the content a consumer would extract rather than on a
// byte count.
func artifactZipEntry(body []byte) (string, error) {
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", deviate("a readable zip archive", truncate(err.Error()),
			"the downloaded artifact is not a zip archive: %v", err)
	}
	for _, file := range archive.File {
		if file.Name != artifactEntryName {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return "", err
		}
		defer reader.Close()
		contents, err := io.ReadAll(reader)
		if err != nil {
			return "", err
		}
		return string(contents), nil
	}
	return "", deviate(artifactEntryName, "absent", "the archive does not contain the entry the job wrote")
}

// fetchArtifactArchive follows the redirect target with the harness's own
// credential, which is what a client does after go-github hands it the
// location.
func fetchArtifactArchive(location *url.URL) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, deviate("200", fmt.Sprintf("%d", response.StatusCode),
			"downloading the artifact archive answered %d: %s", response.StatusCode, truncate(string(body)))
	}
	return body, nil
}

// --- the runner side of the protocol ---------------------------------------

// jitRunner is a registered self-hosted runner: the identity and session a
// real runner holds between `config.sh` and its first job.
type jitRunner struct {
	poolID    int
	agentID   int
	sessionID string
	session   string // the agent session token the assertion exchange produced
}

// leasedJob is one job the broker handed this runner, with the per-job runtime
// token that came with it.
type leasedJob struct {
	runner    *jitRunner
	requestID int64
	token     string
}

// jitConfig is the subset of the runner's just-in-time configuration this
// harness reads: who the runner is, and the key it authenticates with.
type jitConfig struct {
	Runner struct {
		AgentID int `json:"agentId"`
		PoolID  int `json:"poolId"`
	}
	Credentials struct {
		Data struct {
			ClientID         string `json:"clientId"`
			AuthorizationURL string `json:"authorizationUrl"`
		} `json:"data"`
	}
	Key struct {
		D        []byte `json:"d"`
		Exponent []byte `json:"exponent"`
		Modulus  []byte `json:"modulus"`
		P        []byte `json:"p"`
		Q        []byte `json:"q"`
	}
}

// newJITRunner registers an ephemeral runner through the documented REST route
// and completes the credential exchange the official runner performs: the
// just-in-time configuration carries a private key, the key signs a client
// assertion, and the assertion buys an agent session token.
func newJITRunner(client *github.Client, sc *scratch) (*jitRunner, error) {
	config, _, err := client.Actions.GenerateRepoJITConfig(ctx, sc.owner, sc.repo,
		&github.GenerateJITConfigRequest{
			Name:          "conformance-artifact-runner",
			RunnerGroupID: 1,
			Labels:        []string{"self-hosted", "conformance"},
		})
	if err != nil {
		return nil, err
	}
	decoded, err := decodeJITConfig(config.GetEncodedJITConfig())
	if err != nil {
		return nil, err
	}
	key, err := decoded.privateKey()
	if err != nil {
		return nil, err
	}
	session, err := exchangeClientAssertion(decoded.Credentials.Data.AuthorizationURL,
		decoded.Credentials.Data.ClientID, key)
	if err != nil {
		return nil, err
	}
	runner := &jitRunner{poolID: decoded.Runner.PoolID, agentID: decoded.Runner.AgentID, session: session}
	if runner.poolID == 0 {
		runner.poolID = 1
	}
	if err := runner.openSession(); err != nil {
		return nil, err
	}
	return runner, nil
}

// decodeJITConfig unpacks the base64 blob the runner is handed: a map of file
// name to the base64 of that file's contents, which the runner writes verbatim
// into its own root directory.
func decodeJITConfig(encoded string) (*jitConfig, error) {
	if encoded == "" {
		return nil, fmt.Errorf("the just-in-time configuration is empty")
	}
	outer, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode the just-in-time configuration: %w", err)
	}
	var files map[string]string
	if err := json.Unmarshal(outer, &files); err != nil {
		return nil, fmt.Errorf("parse the just-in-time configuration: %w", err)
	}
	config := &jitConfig{}
	sections := map[string]any{
		".runner":                &config.Runner,
		".credentials":           &config.Credentials,
		".credentials_rsaparams": &config.Key,
	}
	for name, target := range sections {
		body, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("the just-in-time configuration has no %s", name)
		}
		contents, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", name, err)
		}
		if err := json.Unmarshal(contents, target); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
	}
	return config, nil
}

// privateKey rebuilds the signing key from the .NET-shaped parameter set the
// configuration carries.
func (c *jitConfig) privateKey() (*rsa.PrivateKey, error) {
	if len(c.Key.Modulus) == 0 || len(c.Key.D) == 0 || len(c.Key.P) == 0 || len(c.Key.Q) == 0 {
		return nil, fmt.Errorf("the just-in-time configuration carries no signing key")
	}
	key := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{
			N: new(big.Int).SetBytes(c.Key.Modulus),
			E: int(new(big.Int).SetBytes(c.Key.Exponent).Int64()),
		},
		D:      new(big.Int).SetBytes(c.Key.D),
		Primes: []*big.Int{new(big.Int).SetBytes(c.Key.P), new(big.Int).SetBytes(c.Key.Q)},
	}
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("the runner's signing key does not validate: %w", err)
	}
	key.Precompute()
	return key, nil
}

// exchangeClientAssertion performs the OAuth exchange the runner performs on
// every start: a JSON Web Token signed by the runner's key, for a session
// access token.
func exchangeClientAssertion(authorizationURL, clientID string, key *rsa.PrivateKey) (string, error) {
	if authorizationURL == "" || clientID == "" {
		return "", fmt.Errorf("the just-in-time configuration names no authorization endpoint")
	}
	assertion, err := signClientAssertion(clientID, key)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, authorizationURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the client assertion exchange answered %d: %s",
			response.StatusCode, truncate(string(body)))
	}
	var issued struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &issued); err != nil {
		return "", fmt.Errorf("parse the issued session token: %w", err)
	}
	if issued.AccessToken == "" {
		return "", fmt.Errorf("the client assertion exchange issued no access token")
	}
	return issued.AccessToken, nil
}

// signClientAssertion builds the RS256 assertion the token endpoint verifies
// against the public key the runner registered.
func signClientAssertion(clientID string, key *rsa.PrivateKey) (string, error) {
	header := base64url([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iss": clientID,
		"sub": clientID,
		"jti": fmt.Sprintf("%d", time.Now().UnixNano()),
		"nbf": time.Now().Add(-time.Minute).Unix(),
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64url(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64url(signature), nil
}

func base64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// runnerRequest issues one runner-protocol call with a runner credential.
func runnerRequest(method, path, credential string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	return response.StatusCode, payload, err
}

// openSession opens the message session a runner keeps for its lifetime.
func (r *jitRunner) openSession() error {
	status, body, err := runnerRequest(http.MethodPost,
		fmt.Sprintf("/_apis/v1/AgentSession/%d", r.poolID), r.session,
		map[string]any{"ownerName": "conformance", "agent": map[string]any{"id": r.agentID}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("opening a runner session answered %d: %s", status, truncate(string(body)))
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		return fmt.Errorf("parse the runner session: %w", err)
	}
	if session.SessionID == "" {
		return fmt.Errorf("the runner session has no identifier")
	}
	r.sessionID = session.SessionID
	return nil
}

// close deletes the session, which is also what deregisters an ephemeral
// runner — the last call a runner makes before it exits.
func (r *jitRunner) close() {
	if r.sessionID == "" {
		return
	}
	_, _, _ = runnerRequest(http.MethodDelete,
		fmt.Sprintf("/_apis/v1/AgentSession/%d/%s", r.poolID, r.sessionID), r.session, nil)
}

// leaseJob long-polls for the dispatched job and reads the runtime token out
// of the job message, exactly where the official runner reads it.
func (r *jitRunner) leaseJob() (*leasedJob, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		status, body, err := runnerRequest(http.MethodGet,
			fmt.Sprintf("/_apis/v1/Message/%d?sessionId=%s", r.poolID, url.QueryEscape(r.sessionID)),
			r.session, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("polling for a job answered %d: %s", status, truncate(string(body)))
		}
		if job, ok, err := r.readJobMessage(body); err != nil {
			return nil, err
		} else if ok {
			return job, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no job message was delivered within the deadline")
		}
	}
}

// readJobMessage decodes one broker message, returning false when the poll
// simply timed out with nothing to deliver.
func (r *jitRunner) readJobMessage(body []byte) (*leasedJob, bool, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false, nil
	}
	var envelope struct {
		MessageType string `json:"messageType"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("parse the broker message: %w", err)
	}
	if envelope.MessageType != "PipelineAgentJobRequest" {
		return nil, false, nil
	}
	var message struct {
		RequestID int64 `json:"requestId"`
		Resources struct {
			Endpoints []struct {
				Name          string `json:"name"`
				Authorization struct {
					Parameters struct {
						AccessToken string `json:"AccessToken"`
					} `json:"parameters"`
				} `json:"authorization"`
			} `json:"endpoints"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(envelope.Body), &message); err != nil {
		return nil, false, fmt.Errorf("parse the job message: %w", err)
	}
	for _, endpoint := range message.Resources.Endpoints {
		if endpoint.Name != "SystemVssConnection" {
			continue
		}
		if endpoint.Authorization.Parameters.AccessToken == "" {
			return nil, false, fmt.Errorf("the job message carries no runtime token")
		}
		return &leasedJob{runner: r, requestID: message.RequestID,
			token: endpoint.Authorization.Parameters.AccessToken}, true, nil
	}
	return nil, false, fmt.Errorf("the job message has no SystemVssConnection endpoint")
}

// twirp issues one call to the artifact service with the job runtime token.
func (j *leasedJob) twirp(method string, request any, response any) error {
	path := "/twirp/github.actions.results.api.v1.ArtifactService/" + method
	status, body, err := runnerRequest(http.MethodPost, path, j.token, request)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return deviate("200", fmt.Sprintf("%d", status), "%s answered %d: %s", method, status, truncate(string(body)))
	}
	if response == nil {
		return nil
	}
	if err := json.Unmarshal(body, response); err != nil {
		return deviate("a decodable "+method+" response", truncate(string(body)),
			"%s did not answer in the shape the toolkit decodes: %v", method, err)
	}
	return nil
}

func (j *leasedJob) createArtifact(backendID, name string) (string, error) {
	var created struct {
		OK              bool   `json:"ok"`
		SignedUploadURL string `json:"signed_upload_url"`
	}
	if err := j.twirp("CreateArtifact", map[string]any{
		"workflow_run_backend_id": backendID,
		"name":                    name,
		"version":                 4,
	}, &created); err != nil {
		return "", err
	}
	if !created.OK {
		return "", deviate("ok true", "ok false", "CreateArtifact refused the artifact")
	}
	return created.SignedUploadURL, nil
}

func (j *leasedJob) uploadArtifact(uploadURL string, payload []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+j.token)
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return deviate("200", fmt.Sprintf("%d", response.StatusCode),
			"uploading the artifact bytes answered %d: %s", response.StatusCode, truncate(string(body)))
	}
	return nil
}

func (j *leasedJob) finalizeArtifact(backendID, name string, size int64) (int64, error) {
	var finalized struct {
		OK         bool  `json:"ok"`
		ArtifactID int64 `json:"artifact_id"`
	}
	if err := j.twirp("FinalizeArtifact", map[string]any{
		"workflow_run_backend_id": backendID,
		"name":                    name,
		"size":                    size,
	}, &finalized); err != nil {
		return 0, err
	}
	if !finalized.OK {
		return 0, deviate("ok true", "ok false", "FinalizeArtifact refused the artifact")
	}
	return finalized.ArtifactID, nil
}

func (j *leasedJob) listArtifacts(backendID string) ([]string, error) {
	var listed struct {
		Artifacts []struct {
			Name string `json:"name"`
		} `json:"artifacts"`
	}
	if err := j.twirp("ListArtifacts", map[string]any{"workflow_run_backend_id": backendID}, &listed); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(listed.Artifacts))
	for _, artifact := range listed.Artifacts {
		names = append(names, artifact.Name)
	}
	return names, nil
}

// complete reports the job running and then succeeded, the two calls the
// runner's listener makes around a job it executed.
func (j *leasedJob) complete() error {
	path := fmt.Sprintf("/_apis/v1/AgentRequest/%d/%d", j.runner.poolID, j.requestID)
	if status, body, err := runnerRequest(http.MethodPatch, path, j.runner.session,
		map[string]any{"requestId": j.requestID}); err != nil {
		return err
	} else if status != http.StatusOK {
		return fmt.Errorf("renewing the job request answered %d: %s", status, truncate(string(body)))
	}
	status, body, err := runnerRequest(http.MethodDelete, path+"?result=succeeded", j.runner.session, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("completing the job request answered %d: %s", status, truncate(string(body)))
	}
	return nil
}
