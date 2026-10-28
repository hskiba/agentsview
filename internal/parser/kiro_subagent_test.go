package parser

import (
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

// parseKiroTestSessions discovers and parses every source under root and
// returns the sessions they emit by session ID. Each source emits at most
// one session.
func parseKiroTestSessions(t *testing.T, root string) map[string]ParseResult {
	t.Helper()
	provider := newKiroTestProvider(t, root)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	byID := make(map[string]ParseResult, len(sources))
	for _, source := range sources {
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
		require.NoError(t, err, "parse %s", source.DisplayPath)
		require.Empty(t, outcome.SourceErrors, "parse %s", source.DisplayPath)
		require.LessOrEqual(t, len(outcome.Results), 1, "parse %s", source.DisplayPath)
		for _, result := range outcome.Results {
			byID[result.Result.Session.ID] = result.Result
		}
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

func TestKiroProviderParsesSubagentTranscriptsAsSubagentSessions(t *testing.T) {
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
	// Blank content is not a record the parser accepts.
	writeKiroTestSubExecution(t, transcript, kiroTestChildD,
		testjsonl.KiroUserJSON("2026-06-01T10:00:05Z", "   "),
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:06Z", ""),
	)

	byID := parseKiroTestSessions(t, root)
	parentID := "kiro:" + kiroTestParentRawID
	require.ElementsMatch(t,
		[]string{parentID, "kiro:" + kiroTestChildA, "kiro:" + kiroTestChildB},
		slices.Collect(maps.Keys(byID)),
		"subagent %s has no accepted records and must not become a session", kiroTestChildD)
	parent := byID[parentID]
	assert.Empty(t, parent.Session.ParentSessionID, "the session is top-level")
	assert.Equal(t, "Parent session", parent.Session.SessionName)

	parentCalls := kiroTestToolCalls(parent)
	assert.Equal(t, []string{kiroTestChildA}, slices.Sorted(maps.Keys(parentCalls)),
		"spawns of ../escape and %s are not valid subagent IDs", kiroTestSessionShapedID)
	spawn := parentCalls[kiroTestChildA]
	assert.Equal(t, "invoke_subagent", spawn.ToolName)
	assert.Equal(t, "Task", spawn.Category)
	assert.Equal(t, "kiro:"+kiroTestChildA, spawn.SubagentSessionID)
	assert.JSONEq(t,
		`{"subagent_type":"coder","description":"delegate the subtask","prompt":"do the subtask"}`,
		spawn.InputJSON)
	assert.Equal(t, map[string]string{kiroTestChildA: `"subtask done"`}, kiroTestToolResults(parent))

	tests := []struct {
		name      string
		subID     string
		firstTurn string
	}{
		{name: "spawned by the session", subID: kiroTestChildA, firstTurn: "child A working"},
		{name: "spawned by another subagent", subID: kiroTestChildB, firstTurn: "child B working"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			child := byID["kiro:"+tt.subID]
			// The subagent linker moves a nested subagent onto the subagent
			// whose Task call names it.
			assert.Equal(t, parentID, child.Session.ParentSessionID, "parent of %s", tt.subID)
			assert.Equal(t, RelSubagent, child.Session.RelationshipType, "relationship of %s", tt.subID)
			assert.Equal(t, kiroTestSubExecutionPath(transcript, tt.subID), child.Session.File.Path)
			assert.Equal(t, parent.Session.Project, child.Session.Project, "project of %s", tt.subID)
			assert.Equal(t, "/home/user/code/kiro-app", child.Session.Cwd, "cwd of %s", tt.subID)
			assert.Equal(t, "devbox", child.Session.Machine, "machine of %s", tt.subID)
			assert.Empty(t, child.Session.SessionName, "the session title belongs to the spawning session")
			require.NotEmpty(t, child.Messages, "messages of %s", tt.subID)
			assert.Equal(t, tt.firstTurn, child.Messages[0].Content,
				"%s starts with its own first record", tt.subID)
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

// A sub_agent_complete record becomes the spawn's tool result. Its content
// falls back to errorMessage when response is empty, and its status maps
// kiro-cli's success and error so tool health can count a failed spawn.
func TestKiroProviderSubagentCompletionResult(t *testing.T) {
	tests := []struct {
		name   string
		subID  string
		fields map[string]any
		want   string // raw result JSON; empty when no result is recorded
		status string
	}{
		{
			name:   "failed spawn with a null response uses the error message",
			subID:  "00000000-0000-4000-8000-000000000001",
			fields: map[string]any{"status": "error", "response": nil, "errorMessage": "e"},
			want:   `"e"`,
			status: "errored",
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
			name:   "successful spawn keeps an object response as JSON",
			subID:  "00000000-0000-4000-8000-000000000004",
			fields: map[string]any{"status": "success", "response": map[string]any{"t": 1}},
			want:   `{"t":1}`,
			status: "completed",
		},
		{
			name:   "unrecognized status leaves the outcome unknown",
			subID:  "00000000-0000-4000-8000-000000000005",
			fields: map[string]any{"status": "pending", "response": ""},
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
	results := map[string]ParsedToolResult{}
	for _, message := range parseKiroTestSessions(t, root)["kiro:"+kiroTestParentRawID].Messages {
		for _, result := range message.ToolResults {
			results[result.ToolUseID] = result
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, recorded := results[tt.subID]
			if tt.want == "" {
				assert.False(t, recorded, "sub_agent_complete for %q recorded result %+v", tt.subID, got)
				return
			}
			require.True(t, recorded, "sub_agent_complete for %q with %v recorded no result", tt.subID, tt.fields)
			assert.Equal(t, tt.want, got.ContentRaw, "result for sub_agent_complete %v", tt.fields)
			assert.Equal(t, "tool_result", got.Source, "source for sub_agent_complete %v", tt.fields)
			assert.Equal(t, tt.status, got.Status, "status for sub_agent_complete %v", tt.fields)
		})
	}
}

func TestKiroProviderSubagentPathsAcrossSeams(t *testing.T) {
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

	t.Run("discover", func(t *testing.T) {
		sources, err := provider.Discover(t.Context())
		require.NoError(t, err)
		var paths []string
		for _, source := range sources {
			paths = append(paths, source.DisplayPath)
		}
		assert.ElementsMatch(t, []string{transcript, child, rootTranscript, rootChild}, paths,
			"only regular <id>.jsonl files directly in a session's sub-executions are subagents")
	})

	tests := []struct {
		name  string
		path  string
		event string
		// wantChanged is the source an event refreshes; empty when none is.
		wantChanged string
		// wantFound is the source a stored path resolves to; empty when
		// rejected. A removed transcript still resolves, so its session can
		// be marked source-missing.
		wantFound string
		symlink   bool
	}{
		{name: "workspace session subagent", path: child, event: "write", wantChanged: child, wantFound: child},
		{name: "root-level session subagent", path: rootChild, event: "write", wantChanged: rootChild, wantFound: rootChild},
		{name: "removed subagent of an existing session", path: removed, event: "remove", wantFound: removed},
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
			var changedPaths, wantChanged []string
			for _, source := range changed {
				changedPaths = append(changedPaths, source.DisplayPath)
			}
			if tt.wantChanged != "" {
				wantChanged = []string{tt.wantChanged}
			}
			assert.Equal(t, wantChanged, changedPaths, "sources for a %s event on %s", tt.event, tt.path)
			var wantScopes []StoredSourceHintScope
			if tt.wantFound != "" {
				wantScopes = []StoredSourceHintScope{{Path: tt.wantFound}}
			}
			assert.Equal(t, wantScopes, scoper.StoredSourceHintScopes(req),
				"stored source scopes for a %s event on %s", tt.event, tt.path)

			found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{StoredFilePath: tt.path})
			require.NoError(t, err, "find stored path %s", tt.path)
			assert.Equal(t, tt.wantFound != "", ok, "find stored path %s", tt.path)
			if ok {
				assert.Equal(t, tt.wantFound, found.DisplayPath, "source for stored path %s", tt.path)
			}
		})
	}
}

// A subagent source covers its own transcript and the working directory it
// takes from session.json, and nothing else in session.json. A session
// source is unaffected by its subagents.
func TestKiroProviderSubagentFingerprint(t *testing.T) {
	root := t.TempDir()
	transcript := writeKiroTestSession(t, root, testjsonl.KiroUserJSON("", "parent prompt"))
	sidecar := filepath.Join(filepath.Dir(transcript), "session.json")
	childText := testjsonl.JoinJSONL(testjsonl.KiroAssistantJSON("", "child v1"))
	childPath := kiroTestSubExecutionPath(transcript, kiroTestChildA)
	writeSourceFile(t, childPath, childText)
	provider := newKiroTestProvider(t, root)
	fingerprint := func(rawID string) SourceFingerprint {
		t.Helper()
		source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: rawID})
		require.NoError(t, err)
		require.True(t, found, "session %s should be found", rawID)
		fp, err := provider.Fingerprint(t.Context(), source)
		require.NoError(t, err, "fingerprint %s", source.DisplayPath)
		return fp
	}

	session := fingerprint(kiroTestParentRawID)
	assert.Equal(t, "998c34daf40686d851baf8ce51dc4b2df6fbc0c0660d3369e2ab4895fdc1ca98", session.Hash,
		"a session's hash does not depend on its subagent transcripts")
	base := fingerprint(kiroTestChildA)
	assert.Equal(t, int64(len(childText)), base.Size)

	// Same size and an older mtime: only the content hash can notice.
	require.NoError(t, os.WriteFile(childPath, []byte(strings.Replace(childText, "v1", "v2", 1)), 0o644))
	older := time.Unix(0, base.MTimeNS).Add(-time.Hour)
	require.NoError(t, os.Chtimes(childPath, older, older))
	rewritten := fingerprint(kiroTestChildA)
	assert.NotEqual(t, base.Hash, rewritten.Hash, "a rewritten subagent transcript changes the hash")
	assert.Equal(t, base.Size, rewritten.Size)

	writeSourceFile(t, sidecar,
		`{"title":"Renamed","lastModifiedAt":"2026-06-02T00:00:00Z","workspacePaths":["/home/user/code/kiro-app"]}`)
	assert.Equal(t, rewritten, fingerprint(kiroTestChildA),
		"a session title or timestamp change does not refresh its subagents")
	assert.NotEqual(t, session.Hash, fingerprint(kiroTestParentRawID).Hash)

	writeSourceFile(t, sidecar, `{"workspacePaths":["/home/user/code/other-app"]}`)
	assert.NotEqual(t, rewritten.Hash, fingerprint(kiroTestChildA).Hash,
		"a subagent takes its working directory from session.json, so changing it refreshes the subagent")
}

// A subagent copied under the same session in several configured roots is
// arbitrated like a session: an event on any copy refreshes the preferred
// one, which here is in the first root.
func TestKiroProviderSubagentChangedPathPrefersConfiguredRootOrder(t *testing.T) {
	preferred, other := t.TempDir(), t.TempDir()
	var copies []string
	for _, root := range []string{preferred, other} {
		transcript := writeKiroTestSession(t, root, testjsonl.KiroUserJSON("", "parent prompt"))
		copies = append(copies, writeKiroTestSubExecution(t, transcript, kiroTestChildA,
			testjsonl.KiroAssistantJSON("", "child")))
	}
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{preferred, other}})
	require.True(t, ok)

	for _, changed := range copies {
		sources, err := provider.SourcesForChangedPath(t.Context(),
			ChangedPathRequest{Path: changed, EventKind: "write"})
		require.NoError(t, err, "event on %s", changed)
		require.Len(t, sources, 1, "event on %s", changed)
		assert.Equal(t, copies[0], sources[0].DisplayPath, "event on %s", changed)
	}
}

// A subagent takes its working directory from its session's session.json,
// so an event on that file refreshes the session and its subagents.
func TestKiroProviderSessionMetadataEventRefreshesSubagents(t *testing.T) {
	root := t.TempDir()
	transcript := writeKiroTestSession(t, root, testjsonl.KiroUserJSON("", "parent prompt"))
	child := writeKiroTestSubExecution(t, transcript, kiroTestChildA, testjsonl.KiroAssistantJSON("", "child"))
	provider := newKiroTestProvider(t, root)

	tests := []struct {
		path string
		want []string
	}{
		{path: filepath.Join(filepath.Dir(transcript), "session.json"), want: []string{transcript, child}},
		{path: transcript, want: []string{transcript}},
		{path: child, want: []string{child}},
	}
	for _, tt := range tests {
		sources, err := provider.SourcesForChangedPath(t.Context(),
			ChangedPathRequest{Path: tt.path, EventKind: "write", WatchRoot: root})
		require.NoError(t, err, "event on %s", tt.path)
		var got []string
		for _, source := range sources {
			got = append(got, source.DisplayPath)
		}
		assert.Equal(t, tt.want, got, "sources for an event on %s", tt.path)
	}
}

func TestKiroProviderFindSourceResolvesSubagentID(t *testing.T) {
	tests := []struct {
		name     string
		breakDir func(t *testing.T, dir string)
	}{
		{name: "other sessions are intact", breakDir: func(*testing.T, string) {}},
		{
			name: "another session's sub-executions is a file",
			breakDir: func(t *testing.T, dir string) {
				t.Helper()
				writeSourceFile(t, dir, "not a directory")
			},
		},
		{
			name: "another session's sub-executions is unreadable",
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
			childPath := writeKiroTestSubExecution(t, transcript, kiroTestChildA, testjsonl.KiroAssistantJSON("", "child"))
			legacy := filepath.Join(root, "legacy-session.jsonl")
			writeSourceFile(t, legacy, kiroProviderJSONLFixture("Legacy question"))
			other := filepath.Join(root, "workspace", "sess_00000000000000ff")
			writeSourceFile(t, filepath.Join(other, "messages.jsonl"),
				testjsonl.JoinJSONL(testjsonl.KiroUserJSON("", "other prompt")))
			tt.breakDir(t, filepath.Join(other, "sub-executions"))
			provider := newKiroTestProvider(t, root)

			for _, want := range []struct {
				req  FindSourceRequest
				path string
			}{
				{req: FindSourceRequest{RawSessionID: kiroTestChildA}, path: childPath},
				{req: FindSourceRequest{FullSessionID: "kiro:" + kiroTestChildA}, path: childPath},
				{req: FindSourceRequest{RawSessionID: kiroTestParentRawID}, path: transcript},
				{req: FindSourceRequest{RawSessionID: "legacy-session"}, path: legacy},
				{req: FindSourceRequest{RawSessionID: kiroTestChildB}},
			} {
				found, ok, err := provider.FindSource(t.Context(), want.req)
				require.NoError(t, err, "find %+v", want.req)
				require.Equal(t, want.path != "", ok, "find %+v", want.req)
				if ok {
					assert.Equal(t, want.path, found.DisplayPath, "find %+v", want.req)
				}
			}
		})
	}
}
