package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

type validateBrowser struct {
	artifacts.Browser
	files map[string][]byte
}

func (b *validateBrowser) Read(_ context.Context, file string, offset, length int) ([]byte, int64, error) {
	content, ok := b.files[file]
	if !ok {
		return nil, -1, errors.New("not found")
	}
	if offset >= len(content) {
		return nil, int64(len(content)), nil
	}
	end := min(offset+length, len(content))
	return content[offset:end], int64(len(content)), nil
}

func TestValidateAnalysisStatus(t *testing.T) {
	tests := []struct {
		name        string
		paths       string
		wantStatus  int
		wantValid   bool
		wantMissing string
	}{
		{name: "all present", paths: `{"paths":["build-log.txt"]}`, wantStatus: http.StatusOK, wantValid: true},
		{name: "missing path", paths: `{"paths":["build-log.txt","missing.log"]}`, wantStatus: http.StatusUnprocessableEntity, wantMissing: "missing.log"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := &toolEnv{browser: &validateBrowser{files: map[string][]byte{"build-log.txt": []byte("x")}}}
			req := httptest.NewRequest(http.MethodPost, "/tool/validate_analysis", strings.NewReader(tt.paths))
			recorder := httptest.NewRecorder()

			validateAnalysis(env, recorder, req)

			response := recorder.Result()
			defer response.Body.Close()
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, tt.wantStatus)
			}
			var result struct {
				AllPresent bool     `json:"all_present"`
				Missing    []string `json:"missing"`
			}
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if result.AllPresent != tt.wantValid {
				t.Errorf("all_present = %t, want %t", result.AllPresent, tt.wantValid)
			}
			if tt.wantMissing != "" && (len(result.Missing) != 1 || result.Missing[0] != tt.wantMissing) {
				t.Errorf("missing = %v, want [%s]", result.Missing, tt.wantMissing)
			}
		})
	}
}

func TestValidateAnalysisEnforcesMatchedSkillEvidence(t *testing.T) {
	set, err := skills.ParseContract([]byte(`{
		"skills":[{
			"id":"quota",
			"triggers":["(?i)quota"],
			"required_evidence":[{"id":"events","any_of":["events/.*quota"]}]
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	header, err := set.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	env := &toolEnv{browser: &validateBrowser{files: map[string][]byte{
		"build-log.txt":          []byte("x"),
		"events/quota-event.log": []byte("x"),
	}}}

	request := func(paths string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/tool/validate_analysis", strings.NewReader(`{"paths":`+paths+`,"analysis":"resource quota exceeded"}`))
		req.Header.Set(skills.ContractHeader, header)
		recorder := httptest.NewRecorder()
		validateAnalysis(env, recorder, req)
		return recorder
	}

	missing := request(`["build-log.txt"]`)
	if missing.Code != http.StatusUnprocessableEntity || !strings.Contains(missing.Body.String(), "quota:events") {
		t.Fatalf("missing evidence response = %d %s", missing.Code, missing.Body.String())
	}
	valid := request(`["build-log.txt","events/quota-event.log"]`)
	if valid.Code != http.StatusOK {
		t.Fatalf("valid evidence response = %d %s", valid.Code, valid.Body.String())
	}
}
