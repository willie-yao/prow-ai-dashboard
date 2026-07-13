// Command orka-copilot-proxy is an experimental OpenAI-compatible reverse proxy
// that lets Orka talk to GitHub Copilot's chat-completions API. Orka's Provider
// CRD can send an Authorization bearer but cannot add custom headers, and the
// Copilot endpoint (api.githubcopilot.com) requires a Copilot-Integration-Id
// header in addition to the bearer. This proxy injects that header and forwards
// everything else unchanged, so an Orka `type: openai` Provider pointed at this
// service reaches Copilot with the caller's copilot_chat PAT.
//
// The bearer is passed through from Orka (from the Provider secretRef); the proxy
// holds no secret of its own.
//
// TEMPORARY: this lives only on the `orka` branch alongside experimental/orka/.
// Remove it when the Orka evaluation concludes or Orka is dropped.
package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	upstream := env("UPSTREAM", "https://api.githubcopilot.com")
	integrationID := env("COPILOT_INTEGRATION_ID", "copilot-developer-cli")
	addr := env("ADDR", ":8080")

	target, err := url.Parse(upstream)
	if err != nil {
		log.Fatalf("parse UPSTREAM %q: %v", upstream, err)
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Normalize the path so both "/chat/completions" and
			// "/v1/chat/completions" reach Copilot's "/chat/completions".
			p := strings.TrimPrefix(req.URL.Path, "/v1")
			if p == "" {
				p = "/"
			}
			req.URL.Path = p
			// The header Copilot requires and Orka cannot set itself.
			req.Header.Set("Copilot-Integration-Id", integrationID)
			// Authorization is forwarded as-is from Orka's Provider secret.
		},
		ModifyResponse: func(resp *http.Response) error {
			log.Printf("↗ %s -> %d", resp.Request.URL.Path, resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("✖ proxy error for %s: %v", r.URL.Path, err)
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", proxy)

	log.Printf("orka-copilot-proxy on %s -> %s (integration-id=%s)", addr, upstream, integrationID)
	log.Fatal(http.ListenAndServe(addr, mux))
}
