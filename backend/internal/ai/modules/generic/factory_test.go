package generic

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func TestFactory_WarnsWhenEvidenceSetOnGenericModule(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *project.Config
		wantLog bool
	}{
		{
			name:    "nil config",
			cfg:     nil,
			wantLog: false,
		},
		{
			name:    "no AI block",
			cfg:     &project.Config{},
			wantLog: false,
		},
		{
			name:    "AI block but no evidence",
			cfg:     &project.Config{AI: &project.AI{Module: "generic"}},
			wantLog: false,
		},
		{
			name:    "empty evidence struct is treated as unset",
			cfg:     &project.Config{AI: &project.AI{Module: "generic", Evidence: &project.Evidence{}}},
			wantLog: false,
		},
		{
			name: "machine_logs set with generic module triggers warning",
			cfg: &project.Config{AI: &project.AI{
				Module:   "generic",
				Evidence: &project.Evidence{MachineLogs: []string{"kubelet.log"}},
			}},
			wantLog: true,
		},
		{
			name: "build_log_patterns set with generic module triggers warning",
			cfg: &project.Config{AI: &project.AI{
				Module:   "generic",
				Evidence: &project.Evidence{BuildLogPatterns: []string{"FAIL"}},
			}},
			wantLog: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(orig)

			m := Factory(tc.cfg)
			if m == nil {
				t.Fatal("Factory returned nil module")
			}
			got := strings.Contains(buf.String(), "ai.evidence is set")
			if got != tc.wantLog {
				t.Errorf("warning emitted=%v, want %v; log output: %q", got, tc.wantLog, buf.String())
			}
		})
	}
}
