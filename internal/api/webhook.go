package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/JumpKickOSS/ka/internal/engine"
)

// GitHubHookPath receives GitHub push webhooks (PRD §10.1: webhooks are
// preferred to polling for remote daemons). GitHub cannot send the API
// token, so the request is authenticated by its X-Hub-Signature-256 HMAC
// with the configured secret instead.
const GitHubHookPath = "/api/v1/hooks/github"

func (s *Server) githubHook(w http.ResponseWriter, r *http.Request) {
	secret := s.opts.WebhookSecret
	if secret == "" {
		writeProblem(w, newProblem(codeForbidden, "start the daemon with --webhook-secret-file (or $KA_WEBHOOK_SECRET) and use the same secret in GitHub", "GitHub webhooks are not configured"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
	if err != nil {
		writeProblem(w, newProblem(engine.CodeInvalid, "", "reading the body: "+err.Error()))
		return
	}
	sig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if sig == "" || !hmac.Equal([]byte(sig), []byte(want)) {
		w.Header().Set("WWW-Authenticate", `HMAC realm="ka"`)
		writeProblem(w, newProblem(codeUnauthorized, "the secret in GitHub must match the daemon's --webhook-secret-file", "bad or missing X-Hub-Signature-256"))
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	resp := map[string]any{"event": event}
	switch event {
	case "ping":
		resp["ok"] = true
	case "push":
		var p struct {
			Repository struct {
				CloneURL string `json:"clone_url"`
				SSHURL   string `json:"ssh_url"`
				GitURL   string `json:"git_url"`
				HTMLURL  string `json:"html_url"`
			} `json:"repository"`
		}
		json.Unmarshal(body, &p)
		stores := s.svc.StoresForRepo(p.Repository.CloneURL, p.Repository.SSHURL, p.Repository.GitURL, p.Repository.HTMLURL)
		for _, st := range stores {
			s.svc.Kick(st)
		}
		if stores == nil {
			stores = []string{}
		}
		resp["fetching"] = stores
	default:
		resp["ignored"] = true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(resp)
}
