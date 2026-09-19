package repositories

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fakeValidator is a deterministic changeSetValidator double. Tests control
// the exact result (PASS/FAIL/NOT_AVAILABLE) and count invocations so they can
// assert when validation actually ran.
type fakeValidator struct {
	result *ValidationResult
	err    error
	calls  int
}

func (v *fakeValidator) Run(ctx context.Context, workspaceDir string) (*ValidationResult, error) {
	v.calls++
	if v.err != nil {
		return nil, v.err
	}
	if v.result == nil {
		return &ValidationResult{Status: validationStatusPass, Summary: "fake pass"}, nil
	}
	return v.result, nil
}

// seedChangeSetWorkspace seeds the real cloned workspace plus a go.mod so the
// genuine Go validation pipeline can run.
func (f *issuePipelineFixture) seedChangeSetWorkspace(t *testing.T) string {
	t.Helper()

	dir := f.seedWorkspace(t)

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(
		"module example.com/demo\n\ngo 1.21\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	return dir
}

// generateChangeSet posts to generate-changes for the seeded issue and returns
// the durable change-set id. State (workspace/analysis/proposal) must already
// be in place.
func (f *issuePipelineFixture) generateChangeSet(t *testing.T) uuid.UUID {
	t.Helper()

	issueID := f.store.issues[issueKey{cloneSelected, 9001}].ID

	path := "/repositories/" + cloneSelected.String() +
		"/issues/" + issueID.String() + "/generate-changes"

	recorder := issuePipelineCall(t, f.cloneFixture, f.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusOK {
		t.Fatalf("generate-changes status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var response generateChangesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode generate response: %v", err)
	}

	if response.ChangeSetID == uuid.Nil {
		t.Fatal("generate-changes did not return a change_set_id")
	}

	return response.ChangeSetID
}

// seedGeneratedChangeSet runs the full generate-changes flow over a seeded
// workspace/issue/analysis/proposal and returns the durable change-set id.
func (f *issuePipelineFixture) seedGeneratedChangeSet(t *testing.T) uuid.UUID {
	t.Helper()

	f.seedChangeSetWorkspace(t)

	issueID := f.seedIssue(t, "Fix the Alpha flow")
	f.seedAnalysis(t, issueID)
	f.seedProposal(t, issueID)

	return f.generateChangeSet(t)
}

// changeSetPath builds the approve/apply/validate route for one change set.
func changeSetPath(issueID, changeSetID uuid.UUID, action string) string {
	return "/repositories/" + cloneSelected.String() +
		"/issues/" + issueID.String() +
		"/changes/" + changeSetID.String() + "/" + action
}

// changeSetCall posts to an approve/apply/validate route and decodes the
// state response.
func changeSetCall(t *testing.T, fixture *cloneFixture, path string) (*httptest.ResponseRecorder, changeSetStateResponse) {
	t.Helper()

	recorder := issuePipelineCall(t, fixture, fixture.tokenFor(t, cloneUserID), path)

	var state changeSetStateResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
			t.Fatalf("decode change-set response: %v", err)
		}
	}

	return recorder, state
}

func TestChangeSetWorkflow_HandlerContract(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID

	approvePath := changeSetPath(issueID, changeSetID, "approve")
	applyPath := changeSetPath(issueID, changeSetID, "apply")
	validatePath := changeSetPath(issueID, changeSetID, "validate")

	foreignIssueID := uuid.MustParse("cccccccc-0f00-0000-0000-000000000001")

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		for _, path := range []string{approvePath, applyPath, validatePath} {
			recorder := issuePipelineCall(t, fixture.cloneFixture, "", path)
			if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
				t.Errorf("%s: status=%d body=%s, want uniform unauthorized", path, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("malformed ids are invalid_request", func(t *testing.T) {
		for _, path := range []string{
			"/repositories/not-a-uuid/issues/" + issueID.String() + "/changes/" + changeSetID.String() + "/approve",
			"/repositories/" + cloneSelected.String() + "/issues/not-a-uuid/changes/" + changeSetID.String() + "/approve",
			"/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/changes/not-a-uuid/approve",
		} {
			recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)
			if recorder.Code != http.StatusBadRequest || strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
				t.Errorf("%s: status=%d body=%s, want invalid_request", path, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("foreign and unknown repositories are opaque 404", func(t *testing.T) {
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			path := "/repositories/" + target.String() +
				"/issues/" + issueID.String() +
				"/changes/" + changeSetID.String() + "/approve"
			recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)
			if recorder.Code != http.StatusNotFound || strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("change set not owned by the issue is opaque 404", func(t *testing.T) {
		path := changeSetPath(foreignIssueID, changeSetID, "approve")
		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)
		if recorder.Code != http.StatusNotFound || strings.TrimSpace(recorder.Body.String()) != `{"error":"change_set_not_found"}` {
			t.Errorf("status=%d body=%s, want change_set_not_found", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("unknown change set id is change_set_not_found", func(t *testing.T) {
		unknown := uuid.MustParse("dddddddd-0f00-0000-0000-000000000001")
		path := changeSetPath(issueID, unknown, "approve")
		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)
		if recorder.Code != http.StatusNotFound || strings.TrimSpace(recorder.Body.String()) != `{"error":"change_set_not_found"}` {
			t.Errorf("status=%d body=%s, want change_set_not_found", recorder.Code, recorder.Body.String())
		}
	})
}

func TestChangeSet_ApprovalHappyPathAndIdempotency(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID

	path := changeSetPath(issueID, changeSetID, "approve")

	recorder, state := changeSetCall(t, fixture.cloneFixture, path)
	if recorder.Code != http.StatusOK || state.Status != changeSetStatusApproved {
		t.Fatalf("approve: status=%d state=%q body=%s", recorder.Code, state.Status, recorder.Body.String())
	}
	if state.ChangeSetID != changeSetID || state.ApprovedAt == nil {
		t.Errorf("approve state wrong: id=%s approved_at=%v", state.ChangeSetID, state.ApprovedAt)
	}

	// Idempotent re-approval: 200 with the same approved state, no error.
	recorder2, state2 := changeSetCall(t, fixture.cloneFixture, path)
	if recorder2.Code != http.StatusOK || state2.Status != changeSetStatusApproved {
		t.Errorf("re-approve: status=%d state=%q want approved", recorder2.Code, state2.Status)
	}
	if len(state2.Changes) == 0 {
		t.Error("re-approve should keep returning the reviewed changes")
	}

	// Approve must not touch the workspace nor call the AI service.
	if calls := fixture.calls(); len(calls) != 1 {
		t.Errorf("ai service received %d calls, want 1 (generate only)", len(calls))
	}
}

func TestChangeSet_ApplyRequiresApproval(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID

	path := changeSetPath(issueID, changeSetID, "apply")
	recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusConflict || strings.TrimSpace(recorder.Body.String()) != `{"error":"change_set_not_approved"}` {
		t.Fatalf("apply on generated: status=%d body=%s, want change_set_not_approved", recorder.Code, recorder.Body.String())
	}
}

func TestChangeSet_ValidateRequiresApplied(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID

	recorder, _ := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "approve"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	path := changeSetPath(issueID, changeSetID, "validate")
	recorder = issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusConflict || strings.TrimSpace(recorder.Body.String()) != `{"error":"change_set_invalid_state"}` {
		t.Fatalf("validate before apply: status=%d body=%s, want change_set_invalid_state", recorder.Code, recorder.Body.String())
	}
}

func TestChangeSet_ApplyWritesExactApprovedContent(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID
	dir := fixture.service.workspaces.Dir(cloneSelected)

	recorder, _ := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "approve"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder, state := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "apply"))
	if recorder.Code != http.StatusOK || state.Status != changeSetStatusApplied {
		t.Fatalf("apply: status=%d state=%q body=%s", recorder.Code, state.Status, recorder.Body.String())
	}
	if state.AppliedAt == nil {
		t.Error("applied_at not stamped")
	}

	// The workspace now holds the EXACT approved content — no AI
	// reinterpretation, and the reviewed test file is present.
	content, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if strings.Contains(string(content), "SOURCE_MARKER") || !strings.Contains(string(content), "FIXED") {
		t.Errorf("main.go content not the approved content:\n%s", content)
	}

	testContent, err := os.ReadFile(filepath.Join(dir, "alpha_extra_test.go"))
	if err != nil {
		t.Fatalf("added test file missing after apply: %v", err)
	}
	if !strings.Contains(string(testContent), "TestAlphaExtra") {
		t.Errorf("alpha_extra_test.go has unexpected content:\n%s", testContent)
	}

	// Apply is idempotent: repeating it reuses the applied state and does not
	// re-modify the workspace.
	recorder2, state2 := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "apply"))
	if recorder2.Code != http.StatusOK || state2.Status != changeSetStatusApplied {
		t.Errorf("re-apply: status=%d state=%q want applied", recorder2.Code, state2.Status)
	}
	if contentAfter, _ := os.ReadFile(filepath.Join(dir, "main.go")); string(contentAfter) != string(content) {
		t.Error("idempotent re-apply modified the applied content")
	}

	// approve/apply/validate must never call the AI service; only generate did.
	if calls := fixture.calls(); len(calls) != 1 {
		t.Errorf("ai service received %d calls, want 1 (generate only)", len(calls))
	}
}

func TestChangeSet_WorkspaceChangedRefusesApply(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID
	dir := fixture.service.workspaces.Dir(cloneSelected)

	recorder, _ := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "approve"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// The workspace drifts after approval.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(
		"package demo\n\nfunc Alpha() {\n\t// newer work by the developer\n}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	path := changeSetPath(issueID, changeSetID, "apply")
	recorder = issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusConflict || strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_changed"}` {
		t.Fatalf("apply after drift: status=%d body=%s, want workspace_changed", recorder.Code, recorder.Body.String())
	}

	// The drifted content was preserved untouched.
	content, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(content), "newer work") {
		t.Errorf("drifted content was modified by the refused apply:\n%s", content)
	}
}

func TestChangeSet_WorkspaceChangedAfterGenerationRefusesApprove(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID
	dir := fixture.service.workspaces.Dir(cloneSelected)

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(
		"package demo\n\nfunc Alpha() {\n\t// drifted before approval\n}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	path := changeSetPath(issueID, changeSetID, "approve")
	recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusConflict || strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_changed"}` {
		t.Fatalf("approve after drift: status=%d body=%s, want workspace_changed", recorder.Code, recorder.Body.String())
	}
}

// TestChangeSet_ApplyRollsBackWritesOnFailure proves the all-or-nothing apply:
// a read-only directory makes the SECOND planned write fail after the first
// write has landed — the first write must be rolled back and the change set
// must not be marked applied.
func TestChangeSet_ApplyRollsBackWritesOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only-directory failure injection does not apply as root")
	}

	fixture := newIssuePipelineFileFixture(t)
	fixture.seedChangeSetWorkspace(t)
	dir := fixture.service.workspaces.Dir(cloneSelected)

	original, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("read original main.go: %v", err)
	}

	// A read-only directory forces the second write to fail mid-apply.
	if err := os.MkdirAll(filepath.Join(dir, "locked"), 0o500); err != nil {
		t.Fatalf("mkdir read-only: %v", err)
	}

	// The change set also adds a file inside the read-only directory, so the
	// second physical write must fail after the first (main.go) has landed.
	fixture.overrideAIResponse("/v1/generate-changes", `{"summary":"fix the Alpha flow","changes":[
		{"file_path":"main.go","operation":"modify","symbol":"Alpha","rationale":"fix the Alpha body","original_context":"func Alpha() {","proposed_content":"package demo\n\nimport \"fmt\"\n\nfunc Alpha() {\n\tfmt.Println(\"FIXED\")\n}\n"},
		{"file_path":"locked/new.go","operation":"add","symbol":"New","rationale":"blocked write","proposed_content":"package demo\n"}
	],"tests":[],"assumptions":["the marker is the bug"],"uncertainty":"moderate","status":"generated"}`)

	issueID := fixture.seedIssue(t, "Fix the Alpha flow")
	fixture.seedAnalysis(t, issueID)
	fixture.seedProposal(t, issueID)
	changeSetID := fixture.generateChangeSet(t)

	recorder, _ := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "approve"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), changeSetPath(issueID, changeSetID, "apply"))
	if recorder.Code == http.StatusOK {
		t.Fatalf("apply should have failed, status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// main.go is restored to its pre-apply original and nothing was left behind.
	after, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go after failed apply: %v", err)
	}
	if string(after) != string(original) {
		t.Errorf("main.go not rolled back:\n--- original ---\n%s\n--- after ---\n%s", original, after)
	}
	if _, err := os.Lstat(filepath.Join(dir, "locked", "new.go")); !os.IsNotExist(err) {
		t.Errorf("failed add left a file behind: %v", err)
	}

	// The failed apply must not have marked the set applied.
	set, _, err := fixture.store.GetChangeSetByID(cloneSelected, changeSetID)
	if err != nil {
		t.Fatal(err)
	}
	if set.Status != changeSetStatusApproved {
		t.Errorf("status = %q after failed apply, want still approved", set.Status)
	}
}

func TestChangeSet_ValidateHappyPathRealPipeline(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID

	recorder, _ := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "approve"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder, _ = changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "apply"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// Real controlled validator: the seeded Go module must format, build, vet,
	// and test cleanly after the approved change is applied.
	recorder, state := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "validate"))
	if recorder.Code != http.StatusOK || state.Status != changeSetStatusValidated {
		t.Fatalf("validate: status=%d state=%q body=%s", recorder.Code, state.Status, recorder.Body.String())
	}
	if state.ValidatedAt == nil {
		t.Error("validated_at not stamped")
	}
	if state.Validation == nil || state.Validation.Status != validationStatusPass {
		t.Fatalf("validation result = %+v, want PASS", state.Validation)
	}

	names := make([]string, 0, len(state.Validation.Checks))
	for _, check := range state.Validation.Checks {
		names = append(names, check.Name)
		if check.Status != validationStatusPass {
			t.Errorf("check %q status = %s, want PASS", check.Name, check.Status)
		}
	}
	for _, expected := range supportedGoChecks {
		found := false
		for _, name := range names {
			if name == expected {
				found = true
			}
		}
		if !found {
			t.Errorf("validation missing expected check %q (got %v)", expected, names)
		}
	}

	// Idempotent re-validation: reuses the persisted result, no re-run, no
	// additional AI calls.
	recorder2, state2 := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "validate"))
	if recorder2.Code != http.StatusOK || state2.Status != changeSetStatusValidated || state2.Validation == nil {
		t.Errorf("re-validate: status=%d state=%q validation=%v", recorder2.Code, state2.Status, state2.Validation)
	}

	if calls := fixture.calls(); len(calls) != 1 {
		t.Errorf("ai service received %d calls, want 1 (generate only)", len(calls))
	}
}

func TestChangeSet_ValidateFailedThenRevalidate(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	fixture.service.validator = &fakeValidator{
		result: &ValidationResult{
			Status:   validationStatusFail,
			Failures: 1,
			Summary:  "gofmt -l . reported unformatted files.",
			Checks: []ValidationCheck{
				{Name: "gofmt -l .", Status: validationStatusFail, DurationMs: 3, Message: "main.go"},
			},
		},
	}

	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID

	recorder, _ := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "approve"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve: status=%d", recorder.Code)
	}
	recorder, _ = changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "apply"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder, state := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "validate"))
	if recorder.Code != http.StatusOK || state.Status != changeSetStatusFailed {
		t.Fatalf("validate: status=%d state=%q body=%s", recorder.Code, state.Status, recorder.Body.String())
	}
	if state.Validation == nil || state.Validation.Status != validationStatusFail || state.Validation.Failures != 1 {
		t.Fatalf("validation result = %+v, want FAIL with 1 failure", state.Validation)
	}

	// A failed set may be re-validated; a passing re-run flips it to validated.
	validator := fixture.service.validator.(*fakeValidator)
	validator.result = &ValidationResult{Status: validationStatusPass, Summary: "second run passes"}

	recorder2, state2 := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "validate"))
	if recorder2.Code != http.StatusOK || state2.Status != changeSetStatusValidated {
		t.Fatalf("re-validate after fail: status=%d state=%q body=%s", recorder2.Code, state2.Status, recorder2.Body.String())
	}
	if validator.calls != 2 {
		t.Errorf("validator ran %d times, want 2", validator.calls)
	}
}

func TestChangeSet_ValidatorNotAvailableReportsFailedState(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	fixture.service.validator = &fakeValidator{
		result: &ValidationResult{
			Status:  validationStatusNotAvailable,
			Summary: "No supported build system was detected.",
		},
	}

	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID

	recorder, _ := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "approve"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve: status=%d", recorder.Code)
	}
	recorder, _ = changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "apply"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("apply: status=%d", recorder.Code)
	}

	recorder, state := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "validate"))
	if recorder.Code != http.StatusOK || state.Validation == nil || state.Validation.Status != validationStatusNotAvailable {
		t.Fatalf("validate: status=%d validation=%+v, want NOT_AVAILABLE result", recorder.Code, state.Validation)
	}
	// A not-established validation still lands the state machine in failed
	// (the set is not verified) and stays re-runnable.
	if state.Status != changeSetStatusFailed {
		t.Errorf("state = %q, want failed for an unavailable validation", state.Status)
	}
}

func TestControlledValidator_NotAvailableWithoutMarker(t *testing.T) {
	// seedWorkspace (no go.mod) → the real runner detects no marker and must
	// NOT execute any command, returning NOT_AVAILABLE deterministically.
	fixture := newIssuePipelineFileFixture(t)
	dir := fixture.seedWorkspace(t)

	validator := newControlledValidator()
	result, err := validator.Run(context.Background(), dir)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.Status != validationStatusNotAvailable {
		t.Errorf("status = %q, want NOT_AVAILABLE", result.Status)
	}
	if len(result.Checks) != 0 {
		t.Errorf("no commands should run without a marker, got %d checks", len(result.Checks))
	}
}

func TestChangeSet_RegenerateResetsStateDeterministically(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	first := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID

	recorder, _ := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, first, "approve"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve: status=%d", recorder.Code)
	}

	second := fixture.generateChangeSet(t)
	if second != first {
		t.Errorf("regenerate produced a different change-set id %s != %s, want deterministic identity", second, first)
	}

	// Regeneration resets the workflow to generated: approval must work again.
	recorder, state := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, second, "approve"))
	if recorder.Code != http.StatusOK || state.Status != changeSetStatusApproved {
		t.Fatalf("approve after regenerate: status=%d state=%q body=%s", recorder.Code, state.Status, recorder.Body.String())
	}
}

func TestChangeSet_StateMachineRejectsInvalidTransitions(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	changeSetID := fixture.seedGeneratedChangeSet(t)
	issueID := fixture.store.issues[issueKey{cloneSelected, 9001}].ID

	recorder, _ := changeSetCall(t, fixture.cloneFixture, changeSetPath(issueID, changeSetID, "approve"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve: status=%d", recorder.Code)
	}
	applyPath := changeSetPath(issueID, changeSetID, "apply")
	recorder, _ = changeSetCall(t, fixture.cloneFixture, applyPath)
	if recorder.Code != http.StatusOK {
		t.Fatalf("apply: status=%d", recorder.Code)
	}

	// Nudge the set into validating directly, then apply must refuse to enter
	// an in-progress validation.
	set, _, err := fixture.store.GetChangeSetByID(cloneSelected, changeSetID)
	if err != nil {
		t.Fatal(err)
	}
	set.Status = changeSetStatusValidating

	recorder = issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), applyPath)
	if recorder.Code != http.StatusConflict || strings.TrimSpace(recorder.Body.String()) != `{"error":"change_set_invalid_state"}` {
		t.Fatalf("apply during validating: status=%d body=%s, want change_set_invalid_state", recorder.Code, recorder.Body.String())
	}
}