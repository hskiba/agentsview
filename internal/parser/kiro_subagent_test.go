package parser

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	kiroTestParentRawID = "sess_0123456789abcdef"
	kiroTestChildA      = "11111111-2222-3333-4444-555555555555"
	kiroTestChildB      = "66666666-7777-8888-9999-000000000000"
	kiroTestChildC      = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	kiroTestChildD      = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	// kiroTestSessionShapedID is a subagent ID shaped like a top-level
	// session directory; stored as kiro:<id> it would collide with that
	// session.
	kiroTestSessionShapedID = "sess_fedcba9876543210"

	kiroTestStartRecordID    = "invoke_subagent_toolu_test-sub-agent-start"
	kiroTestCompleteRecordID = "invoke_subagent_toolu_test-sub-agent-complete"
)

// writeKiroTestSession writes a current-layout session with a sidecar under
// root/workspace and returns the path of its messages.jsonl.
func writeKiroTestSession(t *testing.T, root string, parentLines ...string) string {
	t.Helper()
	sessDir := filepath.Join(root, "workspace", kiroTestParentRawID)
	writeSourceFile(t, filepath.Join(sessDir, "session.json"),
		`{"title":"Parent session","workspacePaths":["/home/user/code/kiro-app"]}`)
	transcript := filepath.Join(sessDir, "messages.jsonl")
	writeSourceFile(t, transcript, testjsonl.JoinJSONL(parentLines...))
	return transcript
}

func kiroTestSubExecutionPath(transcript, subID string) string {
	return filepath.Join(filepath.Dir(transcript), "sub-executions", subID+".jsonl")
}

// writeKiroTestSubExecution writes the subagent transcript of subID beside
// the session transcript and returns its path.
func writeKiroTestSubExecution(t *testing.T, transcript, subID string, lines ...string) string {
	t.Helper()
	path := kiroTestSubExecutionPath(transcript, subID)
	writeSourceFile(t, path, testjsonl.JoinJSONL(lines...))
	return path
}

func newKiroTestProvider(t *testing.T, root string) Provider {
	t.Helper()
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(t, ok, "the Kiro provider should be registered")
	return provider
}

// parseKiroTestSessions parses the single session source under root and
// returns its results by session ID.
func parseKiroTestSessions(t *testing.T, root string) map[string]ParseResult {
	t.Helper()
	provider := newKiroTestProvider(t, root)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1, "a session and its subagents are one source")
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err, "parse %s", sources[0].DisplayPath)
	require.Empty(t, outcome.SourceErrors, "parse %s", sources[0].DisplayPath)
	require.NotEmpty(t, outcome.Results, "parse %s", sources[0].DisplayPath)
	assert.Equal(t, "kiro:"+kiroTestParentRawID, outcome.Results[0].Result.Session.ID,
		"the parent session comes first")
	byID := make(map[string]ParseResult, len(outcome.Results))
	for _, result := range outcome.Results {
		byID[result.Result.Session.ID] = result.Result
	}
	return byID
}

func kiroTestToolCalls(result ParseResult) map[string]ParsedToolCall {
	calls := map[string]ParsedToolCall{}
	for _, message := range result.Messages {
		for _, call := range message.ToolCalls {
			calls[call.ToolUseID] = call
		}
	}
	return calls
}

func kiroTestToolResults(result ParseResult) map[string]string {
	results := map[string]string{}
	for _, message := range result.Messages {
		for _, toolResult := range message.ToolResults {
			results[toolResult.ToolUseID] = toolResult.ContentRaw
		}
	}
	return results
}

func TestKiroProviderParsesSubagentTranscriptsAsLinkedChildren(t *testing.T) {
	root := t.TempDir()
	transcript := writeKiroTestSession(t, root,
		testjsonl.KiroUserJSON("2026-06-01T10:00:00Z", "parent prompt"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "2026-06-01T10:00:01Z",
			kiroTestChildA, "coder", "do the subtask"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "2026-06-01T10:00:01Z",
			"../escape", "coder", "ignored spawn"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "2026-06-01T10:00:01Z",
			kiroTestSessionShapedID, "coder", "ignored spawn"),
		testjsonl.KiroSubAgentCompleteJSON(kiroTestCompleteRecordID, "2026-06-01T10:00:09Z",
			kiroTestChildA, map[string]any{"status": "success", "response": "subtask done"}),
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:10Z", "parent reply"),
	)
	writeKiroTestSubExecution(t, transcript, kiroTestChildA,
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:02Z", "child A working"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "2026-06-01T10:00:02Z",
			kiroTestChildB, "reviewer", "review the change"),
		testjsonl.KiroSubAgentCompleteJSON(kiroTestCompleteRecordID, "2026-06-01T10:00:08Z",
			kiroTestChildB, map[string]any{"status": "error", "response": "", "errorMessage": "reviewer failed"}),
	)
	writeKiroTestSubExecution(t, transcript, kiroTestChildB,
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:03Z", "child B working"),
	)
	writeKiroTestSubExecution(t, transcript, kiroTestChildC,
		testjsonl.KiroUserJSON("2026-06-01T10:00:04Z", "orphan prompt"),
	)
	// Blank content is not a record the parser accepts.
	writeKiroTestSubExecution(t, transcript, kiroTestChildD,
		testjsonl.KiroUserJSON("2026-06-01T10:00:05Z", "   "),
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:06Z", ""),
	)

	byID := parseKiroTestSessions(t, root)
	parentID := "kiro:" + kiroTestParentRawID
	require.ElementsMatch(t,
		[]string{parentID, "kiro:" + kiroTestChildA, "kiro:" + kiroTestChildB, "kiro:" + kiroTestChildC},
		slices.Collect(maps.Keys(byID)),
		"subagent %s has no accepted records and must not become a session", kiroTestChildD)
	parent := byID[parentID]

	parentCalls := kiroTestToolCalls(parent)
	assert.Equal(t, []string{kiroTestChildA}, slices.Sorted(maps.Keys(parentCalls)),
		"spawns of ../escape and %s are not valid subagent IDs", kiroTestSessionShapedID)
	require.Contains(t, parentCalls, kiroTestChildA, "the parent should record the spawn of %s", kiroTestChildA)
	spawn := parentCalls[kiroTestChildA]
	assert.Equal(t, "invoke_subagent", spawn.ToolName)
	assert.Equal(t, "Task", spawn.Category)
	assert.Equal(t, "kiro:"+kiroTestChildA, spawn.SubagentSessionID)
	assert.JSONEq(t,
		`{"subagent_type":"coder","description":"delegate the subtask","prompt":"do the subtask"}`,
		spawn.InputJSON)
	assert.Equal(t, map[string]string{kiroTestChildA: `"subtask done"`}, kiroTestToolResults(parent))

	tests := []struct {
		name         string
		subID        string
		parentID     string
		sessionName  string
		firstMessage string
	}{
		{
			name: "spawned by the session", subID: kiroTestChildA, parentID: parentID,
			sessionName: "coder", firstMessage: "do the subtask",
		},
		{
			name: "spawned by a sibling subagent", subID: kiroTestChildB, parentID: "kiro:" + kiroTestChildA,
			sessionName: "reviewer", firstMessage: "review the change",
		},
		{
			name: "never spawned", subID: kiroTestChildC, parentID: parentID,
			sessionName: "", firstMessage: "orphan prompt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Contains(t, byID, "kiro:"+tt.subID, "subagent transcript %s should be emitted", tt.subID)
			child := byID["kiro:"+tt.subID]
			assert.Equal(t, tt.parentID, child.Session.ParentSessionID, "parent of %s", tt.subID)
			assert.Equal(t, RelSubagent, child.Session.RelationshipType, "relationship of %s", tt.subID)
			assert.Equal(t, kiroTestSubExecutionPath(transcript, tt.subID), child.Session.File.Path)
			assert.Equal(t, parent.Session.Project, child.Session.Project, "project of %s", tt.subID)
			assert.Equal(t, "/home/user/code/kiro-app", child.Session.Cwd, "cwd of %s", tt.subID)
			assert.Equal(t, "devbox", child.Session.Machine, "machine of %s", tt.subID)
			assert.Equal(t, tt.sessionName, child.Session.SessionName, "session name of %s", tt.subID)
			assert.Equal(t, tt.firstMessage, child.Session.FirstMessage, "first message of %s", tt.subID)
		})
	}

	childACalls := kiroTestToolCalls(byID["kiro:"+kiroTestChildA])
	require.Contains(t, childACalls, kiroTestChildB, "subagent %s should record its spawn of %s",
		kiroTestChildA, kiroTestChildB)
	assert.Equal(t, "kiro:"+kiroTestChildB, childACalls[kiroTestChildB].SubagentSessionID)
	assert.Equal(t, map[string]string{kiroTestChildB: `"reviewer failed"`},
		kiroTestToolResults(byID["kiro:"+kiroTestChildA]),
		"a failed spawn reports its error message as the result")
}

// kiro-cli writes a subagent's prompt only into the spawning transcript, so
// the subagent's own transcript would otherwise start mid-task.
func TestKiroProviderStartsSubagentTranscriptWithItsPrompt(t *testing.T) {
	root := t.TempDir()
	transcript := writeKiroTestSession(t, root,
		testjsonl.KiroUserJSON("2026-06-01T10:00:00Z", "parent prompt"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "2026-06-01T10:00:01Z",
			kiroTestChildA, "coder", "do the subtask"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "2026-06-01T10:00:01Z",
			kiroTestChildB, "reviewer", "review the change"),
	)
	writeKiroTestSubExecution(t, transcript, kiroTestChildA,
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:02Z", "child A working"),
	)
	writeKiroTestSubExecution(t, transcript, kiroTestChildB,
		testjsonl.KiroUserJSON("2026-06-01T10:00:03Z", "review the change"),
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:04Z", "child B working"),
	)
	writeKiroTestSubExecution(t, transcript, kiroTestChildC,
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:05Z", "orphan working"),
	)

	byID := parseKiroTestSessions(t, root)
	type turn struct{ Role, Content string }
	tests := []struct {
		name      string
		subID     string
		turns     []turn
		userCount int
		startedAt string
		endedAt   string
	}{
		{
			name:  "prompt copied from the spawn",
			subID: kiroTestChildA,
			turns: []turn{
				{Role: "user", Content: "do the subtask"},
				{Role: "assistant", Content: "child A working"},
			},
			userCount: 1,
			startedAt: "2026-06-01T10:00:01Z",
			endedAt:   "2026-06-01T10:00:02Z",
		},
		{
			name:  "recorded prompt kept as written",
			subID: kiroTestChildB,
			turns: []turn{
				{Role: "user", Content: "review the change"},
				{Role: "assistant", Content: "child B working"},
			},
			userCount: 1,
			startedAt: "2026-06-01T10:00:03Z",
			endedAt:   "2026-06-01T10:00:04Z",
		},
		{
			name:      "never spawned so no prompt to copy",
			subID:     kiroTestChildC,
			turns:     []turn{{Role: "assistant", Content: "orphan working"}},
			userCount: 0,
			startedAt: "2026-06-01T10:00:05Z",
			endedAt:   "2026-06-01T10:00:05Z",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Contains(t, byID, "kiro:"+tt.subID, "subagent transcript %s should be emitted", tt.subID)
			child := byID["kiro:"+tt.subID]
			var turns []turn
			for i, msg := range child.Messages {
				assert.Equal(t, i, msg.Ordinal, "ordinals of %s stay contiguous", tt.subID)
				turns = append(turns, turn{Role: string(msg.Role), Content: msg.Content})
			}
			assert.Equal(t, tt.turns, turns, "turns of %s", tt.subID)
			assert.Equal(t, len(tt.turns), child.Session.MessageCount, "message count of %s", tt.subID)
			assert.Equal(t, tt.userCount, child.Session.UserMessageCount, "user message count of %s", tt.subID)
			assert.Equal(t, tt.startedAt, child.Session.StartedAt.UTC().Format(time.RFC3339),
				"start of %s", tt.subID)
			assert.Equal(t, tt.endedAt, child.Session.EndedAt.UTC().Format(time.RFC3339),
				"end of %s", tt.subID)
		})
	}
}

func TestKiroProviderSubagentResultFallsBackToErrorMessage(t *testing.T) {
	tests := []struct {
		name   string
		subID  string
		fields map[string]any
		want   string // raw result JSON; empty when no result is recorded
	}{
		{
			name:   "null response uses the error message",
			subID:  "00000000-0000-4000-8000-000000000001",
			fields: map[string]any{"response": nil, "errorMessage": "e"},
			want:   `"e"`,
		},
		{
			name:   "missing response uses the error message",
			subID:  "00000000-0000-4000-8000-000000000002",
			fields: map[string]any{"errorMessage": "e"},
			want:   `"e"`,
		},
		{
			name:   "string response wins over the error message",
			subID:  "00000000-0000-4000-8000-000000000003",
			fields: map[string]any{"response": "ok", "errorMessage": "e"},
			want:   `"ok"`,
		},
		{
			name:   "object response is kept as JSON",
			subID:  "00000000-0000-4000-8000-000000000004",
			fields: map[string]any{"response": map[string]any{"t": 1}},
			want:   `{"t":1}`,
		},
		{
			name:   "empty response without an error message",
			subID:  "00000000-0000-4000-8000-000000000005",
			fields: map[string]any{"response": ""},
			want:   `""`,
		},
		{
			name:   "invalid subagent ID records no result",
			subID:  "../escape",
			fields: map[string]any{"response": "ok"},
		},
		{
			name:   "session-shaped subagent ID records no result",
			subID:  kiroTestSessionShapedID,
			fields: map[string]any{"response": "ok"},
		},
	}
	lines := []string{testjsonl.KiroUserJSON("2026-06-01T10:00:00Z", "parent prompt")}
	for _, tt := range tests {
		lines = append(lines, testjsonl.KiroSubAgentCompleteJSON(
			kiroTestCompleteRecordID, "2026-06-01T10:00:09Z", tt.subID, tt.fields))
	}
	root := t.TempDir()
	writeKiroTestSession(t, root, lines...)
	results := kiroTestToolResults(parseKiroTestSessions(t, root)["kiro:"+kiroTestParentRawID])

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, recorded := results[tt.subID]
			if tt.want == "" {
				assert.False(t, recorded, "sub_agent_complete for %q recorded result %s", tt.subID, got)
				return
			}
			require.True(t, recorded, "sub_agent_complete for %q with %v recorded no result", tt.subID, tt.fields)
			assert.Equal(t, tt.want, got, "result for sub_agent_complete %v", tt.fields)
		})
	}
}

// kiro-cli writes every subagent transcript flat in sub-executions, so a
// subagent's spawner is found by the sub_agent_start that names it, not by
// where its file sits.
func TestKiroProviderLinksNestedSubagentsToTheirSpawner(t *testing.T) {
	root := t.TempDir()
	parentID := "kiro:" + kiroTestParentRawID
	transcript := writeKiroTestSession(t, root,
		testjsonl.KiroUserJSON("2026-06-01T10:00:00Z", "parent prompt"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "2026-06-01T10:00:01Z",
			kiroTestChildB, "reviewer", "review the change"),
	)
	// Child C's file sorts after child A's, the subagent it spawns. It also
	// names child B, which the session already spawned.
	writeKiroTestSubExecution(t, transcript, kiroTestChildC,
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:02Z", "child C working"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "2026-06-01T10:00:03Z",
			kiroTestChildA, "coder", "do the subtask"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "2026-06-01T10:00:04Z",
			kiroTestChildB, "reviewer", "review the change again"),
	)
	writeKiroTestSubExecution(t, transcript, kiroTestChildA,
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:05Z", "child A working"),
	)
	writeKiroTestSubExecution(t, transcript, kiroTestChildB,
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:06Z", "child B working"),
	)
	writeKiroTestSubExecution(t, transcript, kiroTestChildD,
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "2026-06-01T10:00:07Z",
			kiroTestChildD, "looper", "spawn myself"),
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:08Z", "child D working"),
	)

	byID := parseKiroTestSessions(t, root)
	tests := []struct {
		name       string
		subID      string
		wantParent string
	}{
		{name: "spawner file sorts after its subagent", subID: kiroTestChildA, wantParent: "kiro:" + kiroTestChildC},
		{name: "session spawn outranks a subagent naming the same ID", subID: kiroTestChildB, wantParent: parentID},
		{name: "subagent naming itself links to the session", subID: kiroTestChildD, wantParent: parentID},
		{name: "subagent nobody spawned links to the session", subID: kiroTestChildC, wantParent: parentID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Contains(t, byID, "kiro:"+tt.subID, "subagent transcript %s should be emitted", tt.subID)
			assert.Equal(t, tt.wantParent, byID["kiro:"+tt.subID].Session.ParentSessionID,
				"parent of subagent %s", tt.subID)
		})
	}
}

// Subagent transcript paths are classified by three seams: the session parse
// that lists sub-executions, the watcher's changed-path mapping, and the
// lookup of a stored file path. Every lookalike must be rejected by each.
func TestKiroProviderSubExecutionPathsAcrossSeams(t *testing.T) {
	const (
		rootSessionRawID = kiroTestSessionShapedID
		nestedID         = "00000000-0000-4000-8000-000000000001"
		historyID        = "00000000-0000-4000-8000-000000000002"
		snapshotID       = "00000000-0000-4000-8000-000000000003"
		noSessionDirID   = "00000000-0000-4000-8000-000000000004"
		jsonExtID        = "00000000-0000-4000-8000-000000000005"
		symlinkID        = "00000000-0000-4000-8000-000000000006"
		removedID        = "00000000-0000-4000-8000-000000000007"
		directoryID      = "00000000-0000-4000-8000-000000000008"
	)
	root := t.TempDir()
	childLine := testjsonl.KiroAssistantJSON("2026-06-01T10:00:02Z", "child working")
	transcript := writeKiroTestSession(t, root,
		testjsonl.KiroUserJSON("2026-06-01T10:00:00Z", "parent prompt"))
	sessDir := filepath.Dir(transcript)
	child := writeKiroTestSubExecution(t, transcript, kiroTestChildA, childLine)
	// A session directly under the root, with no workspace directory, whose
	// ID a session-shaped subagent file elsewhere could collide with.
	rootTranscript := filepath.Join(root, rootSessionRawID, "messages.jsonl")
	writeSourceFile(t, rootTranscript, testjsonl.JoinJSONL(
		testjsonl.KiroUserJSON("2026-06-01T11:00:00Z", "root-level prompt")))
	rootChild := writeKiroTestSubExecution(t, rootTranscript, kiroTestChildB, childLine)
	// Kiro keeps copies of sessions in .history and snapshots workspaces.
	for _, workspace := range []string{".history", "snapshots"} {
		writeSourceFile(t, filepath.Join(root, workspace, kiroTestParentRawID, "messages.jsonl"),
			testjsonl.JoinJSONL(testjsonl.KiroUserJSON("2026-06-01T10:00:00Z", "copied prompt")))
	}
	history := filepath.Join(root, ".history", kiroTestParentRawID, "sub-executions", historyID+".jsonl")
	snapshot := filepath.Join(root, "snapshots", kiroTestParentRawID, "sub-executions", snapshotID+".jsonl")
	noSessionDir := filepath.Join(root, "workspace", "sub-executions", noSessionDirID+".jsonl")
	nested := filepath.Join(sessDir, "sub-executions", "nested", nestedID+".jsonl")
	badID := filepath.Join(sessDir, "sub-executions", "bad.id.jsonl")
	jsonExt := filepath.Join(sessDir, "sub-executions", jsonExtID+".json")
	sessionShaped := kiroTestSubExecutionPath(transcript, rootSessionRawID)
	for _, path := range []string{history, snapshot, noSessionDir, nested, badID, jsonExt, sessionShaped} {
		writeSourceFile(t, path, testjsonl.JoinJSONL(childLine))
	}
	outside := filepath.Join(t.TempDir(), symlinkID+".jsonl")
	writeSourceFile(t, outside, testjsonl.JoinJSONL(childLine))
	symlink := kiroTestSubExecutionPath(transcript, symlinkID)
	symlinkErr := os.Symlink(outside, symlink)
	removed := kiroTestSubExecutionPath(transcript, removedID)
	directory := kiroTestSubExecutionPath(transcript, directoryID)
	require.NoError(t, os.MkdirAll(directory, 0o755))

	provider := newKiroTestProvider(t, root)
	scoper, ok := provider.(StoredSourceHintScopeProvider)
	require.True(t, ok, "the Kiro provider should declare stored source hint scopes")

	t.Run("parse", func(t *testing.T) {
		sources, err := provider.Discover(t.Context())
		require.NoError(t, err)
		type emitted struct{ ID, Path string }
		var got []emitted
		for _, source := range sources {
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err, "parse %s", source.DisplayPath)
			assert.Empty(t, outcome.SourceErrors, "parse %s", source.DisplayPath)
			for _, result := range outcome.Results {
				got = append(got, emitted{ID: result.Result.Session.ID, Path: result.Result.Session.File.Path})
			}
		}
		assert.ElementsMatch(t, []emitted{
			{ID: "kiro:" + kiroTestParentRawID, Path: transcript},
			{ID: "kiro:" + kiroTestChildA, Path: child},
			{ID: "kiro:" + rootSessionRawID, Path: rootTranscript},
			{ID: "kiro:" + kiroTestChildB, Path: rootChild},
		}, got, "only regular <id>.jsonl files directly in a session's sub-executions are subagents")
	})

	sessionScopes := []StoredSourceHintScope{
		{Path: transcript},
		{Path: filepath.Join(sessDir, "sub-executions")},
	}
	tests := []struct {
		name       string
		path       string
		event      string
		wantSource string // owning messages.jsonl; empty when rejected
		wantScopes []StoredSourceHintScope
		symlink    bool
	}{
		{
			name: "workspace session subagent", path: child, event: "write",
			wantSource: transcript, wantScopes: sessionScopes,
		},
		{
			name: "root-level session subagent", path: rootChild, event: "write",
			wantSource: rootTranscript, wantScopes: []StoredSourceHintScope{
				{Path: rootTranscript},
				{Path: filepath.Join(root, rootSessionRawID, "sub-executions")},
			},
		},
		{
			name: "removed subagent of an existing session", path: removed, event: "remove",
			wantSource: transcript, wantScopes: sessionScopes,
		},
		{name: "history workspace", path: history, event: "write"},
		{name: "snapshots workspace", path: snapshot, event: "write"},
		{name: "no session directory", path: noSessionDir, event: "write"},
		{name: "nested directory", path: nested, event: "write"},
		{name: "invalid ID", path: badID, event: "write"},
		{name: "json extension", path: jsonExt, event: "write"},
		{name: "session-shaped ID", path: sessionShaped, event: "write"},
		{name: "directory named like a transcript", path: directory, event: "write"},
		{name: "symlink escaping the root", path: symlink, event: "write", symlink: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.symlink && symlinkErr != nil {
				t.Skipf("symlink creation unavailable: %v", symlinkErr)
			}
			req := ChangedPathRequest{Path: tt.path, EventKind: tt.event, WatchRoot: root}
			changed, err := provider.SourcesForChangedPath(t.Context(), req)
			require.NoError(t, err, "%s event for %s", tt.event, tt.path)
			var changedPaths []string
			for _, source := range changed {
				changedPaths = append(changedPaths, source.DisplayPath)
			}
			var wantChanged []string
			if tt.wantSource != "" {
				wantChanged = []string{tt.wantSource}
			}
			assert.Equal(t, wantChanged, changedPaths, "sources for a %s event on %s", tt.event, tt.path)
			assert.Equal(t, tt.wantScopes, scoper.StoredSourceHintScopes(req),
				"stored source scopes for a %s event on %s", tt.event, tt.path)

			found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{StoredFilePath: tt.path})
			require.NoError(t, err, "find stored path %s", tt.path)
			assert.Equal(t, tt.wantSource != "", ok, "find stored path %s", tt.path)
			if ok {
				assert.Equal(t, tt.wantSource, found.DisplayPath, "source for stored path %s", tt.path)
			}
		})
	}
}

func TestKiroProviderFingerprintCoversSubExecutions(t *testing.T) {
	root := t.TempDir()
	transcriptLine := testjsonl.KiroUserJSON("", "parent prompt")
	transcript := writeKiroTestSession(t, root, transcriptLine)
	sidecarInfo, err := os.Stat(filepath.Join(filepath.Dir(transcript), "session.json"))
	require.NoError(t, err)
	provider := newKiroTestProvider(t, root)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: kiroTestParentRawID})
	require.NoError(t, err)
	require.True(t, found, "session %s should be found", kiroTestParentRawID)
	fingerprint := func() SourceFingerprint {
		t.Helper()
		fp, err := provider.Fingerprint(t.Context(), source)
		require.NoError(t, err, "fingerprint %s", source.DisplayPath)
		return fp
	}

	base := fingerprint()
	assert.Equal(t, "998c34daf40686d851baf8ce51dc4b2df6fbc0c0660d3369e2ab4895fdc1ca98", base.Hash,
		"recorded before subagent transcripts were supported: if a session without them changes "+
			"hash, every archived Kiro session reparses")
	assert.Equal(t, int64(len(transcriptLine)+1)+sidecarInfo.Size(), base.Size)

	writeSourceFile(t, filepath.Join(filepath.Dir(transcript), "sub-executions", "bad.id.jsonl"), "{}\n")
	assert.Equal(t, base, fingerprint(), "files that are not subagent transcripts are ignored")

	childText := testjsonl.JoinJSONL(testjsonl.KiroAssistantJSON("", "child v1"))
	childPath := kiroTestSubExecutionPath(transcript, kiroTestChildA)
	writeSourceFile(t, childPath, childText)
	withChild := fingerprint()
	assert.NotEqual(t, base.Hash, withChild.Hash, "a new subagent transcript changes the hash")
	assert.Equal(t, base.Size+int64(len(childText)), withChild.Size)

	// Same size and an older mtime: only the content hash can notice.
	require.NoError(t, os.WriteFile(childPath, []byte(strings.Replace(childText, "v1", "v2", 1)), 0o644))
	older := time.Unix(0, base.MTimeNS).Add(-time.Hour)
	require.NoError(t, os.Chtimes(childPath, older, older))
	rewritten := fingerprint()
	assert.NotEqual(t, withChild.Hash, rewritten.Hash, "a rewritten subagent transcript changes the hash")
	assert.Equal(t, withChild.Size, rewritten.Size)

	newer := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, os.Chtimes(childPath, newer, newer))
	assert.Equal(t, newer.UnixNano(), fingerprint().MTimeNS,
		"a subagent transcript newer than the session sets the source mtime")

	require.NoError(t, os.Remove(childPath))
	removed := fingerprint()
	assert.Equal(t, base.Hash, removed.Hash, "removing the only subagent transcript restores the session hash")
	assert.Equal(t, base.Size, removed.Size, "removing the only subagent transcript restores the session size")
}

func TestKiroProviderFindSourceResolvesSubagentToItsSession(t *testing.T) {
	root := t.TempDir()
	transcript := writeKiroTestSession(t, root, testjsonl.KiroUserJSON("", "parent prompt"))
	childPath := writeKiroTestSubExecution(t, transcript, kiroTestChildA, testjsonl.KiroAssistantJSON("", "child"))
	provider := newKiroTestProvider(t, root)

	tests := []struct {
		name string
		req  FindSourceRequest
		want bool
	}{
		{name: "raw subagent session ID", req: FindSourceRequest{RawSessionID: kiroTestChildA}, want: true},
		{name: "full subagent session ID", req: FindSourceRequest{FullSessionID: "kiro:" + kiroTestChildA}, want: true},
		{name: "stored subagent file path", req: FindSourceRequest{StoredFilePath: childPath}, want: true},
		{name: "unknown subagent session ID", req: FindSourceRequest{RawSessionID: kiroTestChildB}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, ok, err := provider.FindSource(t.Context(), tt.req)
			require.NoError(t, err, "find %+v", tt.req)
			require.Equal(t, tt.want, ok, "find %+v", tt.req)
			if tt.want {
				assert.Equal(t, transcript, found.DisplayPath,
					"a subagent resolves to the session source that parses it")
			}
		})
	}
}

// A subagent probe that fails for a reason other than absence, such as a
// symlink loop, must surface instead of reading as "no such subagent".
func TestKiroProviderFindSourceReportsSubagentProbeErrors(t *testing.T) {
	root := t.TempDir()
	transcript := writeKiroTestSession(t, root, testjsonl.KiroUserJSON("", "parent prompt"))
	loop := kiroTestSubExecutionPath(transcript, kiroTestChildA)
	require.NoError(t, os.MkdirAll(filepath.Dir(loop), 0o755))
	if err := os.Symlink(loop, loop); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	provider := newKiroTestProvider(t, root)

	_, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: kiroTestChildA})

	var pathErr *os.PathError
	require.ErrorAs(t, err, &pathErr, "finding %s through the looping %s should fail", kiroTestChildA, loop)
	assert.Equal(t, loop, pathErr.Path, "the error names the probed subagent transcript")
	assert.False(t, found)
}

// Fingerprinting and parsing a session read every subagent transcript, so a
// cancellation that arrives after either has started must stop that work.
func TestKiroProviderSubExecutionWorkStopsWhenCancelled(t *testing.T) {
	root := t.TempDir()
	transcript := writeKiroTestSession(t, root, testjsonl.KiroUserJSON("", "parent prompt"))
	writeKiroTestSubExecution(t, transcript, kiroTestChildA, testjsonl.KiroAssistantJSON("", "child A working"))
	writeKiroTestSubExecution(t, transcript, kiroTestChildB, testjsonl.KiroAssistantJSON("", "child B working"))
	provider := newKiroTestProvider(t, root)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1, "a session and its subagents are one source")

	t.Run("parse", func(t *testing.T) {
		// The first cancellation check passes; the context is cancelled by
		// the second.
		_, err := provider.Parse(newCancelOnErrCheckContext(t, 2), ParseRequest{Source: sources[0]})
		require.ErrorIs(t, err, context.Canceled, "parse %s", sources[0].DisplayPath)
	})
	t.Run("fingerprint", func(t *testing.T) {
		_, err := provider.Fingerprint(newCancelOnErrCheckContext(t, 2), sources[0])
		require.ErrorIs(t, err, context.Canceled, "fingerprint %s", sources[0].DisplayPath)
	})
}

// A subagent transcript that cannot be read fails only that subagent and the
// subagents no readable transcript spawns. Each error is keyed under the
// session transcript, the source being parsed, because the engine discards a
// whole outcome whose diagnostic names another source.
func TestKiroProviderUnreadableSubagentFailsOnlyThatSubagent(t *testing.T) {
	root := t.TempDir()
	transcript := writeKiroTestSession(t, root,
		testjsonl.KiroUserJSON("", "parent prompt"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "", kiroTestChildA, "coder", "task A"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "", kiroTestChildB, "coder", "task B"))
	writeKiroTestSubExecution(t, transcript, kiroTestChildA,
		testjsonl.KiroAssistantJSON("", "child A working"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "", kiroTestChildD, "coder", "readable nested task"))
	unreadable := writeKiroTestSubExecution(t, transcript, kiroTestChildB,
		testjsonl.KiroAssistantJSON("", "child B working"),
		testjsonl.KiroSubAgentStartJSON(kiroTestStartRecordID, "", kiroTestChildC, "coder", "nested task"))
	nested := writeKiroTestSubExecution(t, transcript, kiroTestChildC, testjsonl.KiroAssistantJSON("", "child C working"))
	writeKiroTestSubExecution(t, transcript, kiroTestChildD, testjsonl.KiroAssistantJSON("", "child D working"))
	require.NoError(t, os.Chmod(unreadable, 0))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(unreadable, 0o644)) })
	if _, err := os.ReadFile(unreadable); err == nil {
		t.Skip("file permissions do not block reads for this user")
	}
	provider := newKiroTestProvider(t, root)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1, "a session and its subagents are one source")

	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err, "fingerprint %s", sources[0].DisplayPath)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
	require.NoError(t, err, "parse %s", sources[0].DisplayPath)

	var ids []string
	for _, result := range outcome.Results {
		ids = append(ids, result.Result.Session.ID)
	}
	assert.Equal(t, []string{"kiro:" + kiroTestParentRawID, "kiro:" + kiroTestChildA, "kiro:" + kiroTestChildD}, ids,
		"the session and every subagent with a readable spawn record still parse")
	require.Len(t, outcome.SourceErrors, 2)
	for i, want := range []struct {
		subID, path string
		err         error
	}{
		{subID: kiroTestChildB, path: unreadable, err: os.ErrPermission},
		{subID: kiroTestChildC, path: nested, err: errKiroSpawnRecordUnreadable},
	} {
		got := outcome.SourceErrors[i]
		assert.Equal(t, transcript+"#"+want.subID, got.SourceKey)
		assert.Equal(t, want.path, got.DisplayPath)
		assert.Equal(t, "kiro:"+want.subID, got.SessionID)
		assert.True(t, got.Retryable, "subagent %s is retried on the next sync", want.subID)
		assert.ErrorIs(t, got.Err, want.err)
	}
}

// Memory admission charges a parse for every file it reads. Only sizes
// matter here, so each file is filler of the listed size; metadata files hold
// a padded title because discovery decodes them.
func TestKiroProviderParseSourceSizeCoversEveryParsedFile(t *testing.T) {
	session := filepath.Join("workspace", kiroTestParentRawID)
	subExecutions := filepath.Join(session, "sub-executions")
	tests := []struct {
		name  string
		files map[string]int
		want  int64
	}{
		{
			name: "session with subagent transcripts",
			files: map[string]int{
				filepath.Join(session, "messages.jsonl"):                       1000,
				filepath.Join(session, "session.json"):                         200,
				filepath.Join(subExecutions, kiroTestChildA+".jsonl"):          30,
				filepath.Join(subExecutions, kiroTestChildB+".jsonl"):          4,
				filepath.Join(subExecutions, "bad.id.jsonl"):                   50000,
				filepath.Join(subExecutions, kiroTestChildC+".txt"):            50000,
				filepath.Join(subExecutions, kiroTestSessionShapedID+".jsonl"): 50000,
			},
			want: 1234,
		},
		{
			name: "session without subagent transcripts",
			files: map[string]int{
				filepath.Join(session, "messages.jsonl"): 1000,
				filepath.Join(session, "session.json"):   200,
			},
			want: 1200,
		},
		{
			name: "legacy transcript and its metadata",
			files: map[string]int{
				"legacy-session.jsonl": 1000,
				"legacy-session.json":  200,
			},
			want: 1200,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for name, size := range tt.files {
				content := strings.Repeat("x", size)
				if filepath.Ext(name) == ".json" {
					content = `{"title":"` + strings.Repeat("x", size-len(`{"title":""}`)) + `"}`
				}
				writeSourceFile(t, filepath.Join(root, name), content)
			}
			provider := newKiroTestProvider(t, root)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			require.Implements(t, (*ParseSourceSizer)(nil), provider)

			size, err := provider.(ParseSourceSizer).ParseSourceSize(t.Context(), sources[0])

			require.NoError(t, err, "size %s", sources[0].DisplayPath)
			assert.Equal(t, tt.want, size)
		})
	}
}

// A Kiro SQLite source is charged the whole database file.
func TestKiroProviderParseSourceSizeChargesTheWholeSQLiteStore(t *testing.T) {
	root := t.TempDir()
	dbPath, db := newKiroProviderSQLiteDBAt(t, root)
	seedKiroSQLiteSession(t, db, "/home/user/code/kiro-app", "sqlite-session",
		readKiroFixture(t, "standard_payload.json"), 1779012000000, 1779012030000)
	info, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Positive(t, info.Size())
	provider := newKiroTestProvider(t, root)
	physical, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, physical, 1)
	member, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "sqlite-session"})
	require.NoError(t, err)
	require.True(t, found)
	require.Implements(t, (*ParseSourceSizer)(nil), provider)

	for _, source := range []SourceRef{physical[0], member} {
		size, err := provider.(ParseSourceSizer).ParseSourceSize(t.Context(), source)
		require.NoError(t, err, "size %s", source.DisplayPath)
		assert.Equal(t, info.Size(), size, "size %s", source.DisplayPath)
	}
}

// FindSource probes every session's sub-executions directory for IDs shaped
// like a subagent, which includes legacy session names. One session whose
// directory cannot be probed must not hide a source found elsewhere.
func TestKiroProviderFindSourceIgnoresOtherSessionsProbeFailures(t *testing.T) {
	tests := []struct {
		name     string
		breakDir func(t *testing.T, dir string)
	}{
		{
			name: "sub-executions is a file",
			breakDir: func(t *testing.T, dir string) {
				t.Helper()
				writeSourceFile(t, dir, "not a directory")
			},
		},
		{
			name: "sub-executions is unreadable",
			breakDir: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.Chmod(dir, 0))
				t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, 0o755)) })
				if _, err := os.Stat(filepath.Join(dir, kiroTestChildA+".jsonl")); !errors.Is(err, os.ErrPermission) {
					t.Skip("directory permissions do not block lookups for this user")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			transcript := writeKiroTestSession(t, root, testjsonl.KiroUserJSON("", "parent prompt"))
			writeKiroTestSubExecution(t, transcript, kiroTestChildA, testjsonl.KiroAssistantJSON("", "child"))
			legacy := filepath.Join(root, "legacy-session.jsonl")
			writeSourceFile(t, legacy, kiroProviderJSONLFixture("Legacy question"))
			other := filepath.Join(root, "workspace", "sess_00000000000000ff")
			writeSourceFile(t, filepath.Join(other, "messages.jsonl"),
				testjsonl.JoinJSONL(testjsonl.KiroUserJSON("", "other prompt")))
			tt.breakDir(t, filepath.Join(other, "sub-executions"))
			provider := newKiroTestProvider(t, root)

			for _, want := range []struct{ rawID, path string }{
				{rawID: kiroTestChildA, path: transcript},
				{rawID: "legacy-session", path: legacy},
			} {
				found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: want.rawID})
				require.NoError(t, err, "find %s", want.rawID)
				require.True(t, ok, "find %s", want.rawID)
				assert.Equal(t, want.path, found.DisplayPath, "find %s", want.rawID)
			}
		})
	}
}
