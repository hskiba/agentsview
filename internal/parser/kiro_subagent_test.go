package parser

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	kiroTestParentRawID = "sess_0123456789abcdef"
	kiroTestChildA      = "11111111-2222-3333-4444-555555555555"
	kiroTestChildB      = "66666666-7777-8888-9999-000000000000"
	kiroTestOrphan      = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func kiroTestSubAgentStart(subID, name, prompt string) string {
	return `{"id":"invoke_subagent_toolu_test-sub-agent-start","timestamp":"2026-06-01T10:00:01Z","payload":{"type":"sub_agent_start","parentExecutionId":"exec-1","subSessionId":"` + subID + `","subAgentName":"` + name + `","prompt":"` + prompt + `","explanation":"delegate the subtask"}}`
}

func kiroTestSubAgentComplete(subID, fields string) string {
	return `{"id":"invoke_subagent_toolu_test-sub-agent-complete","timestamp":"2026-06-01T10:00:09Z","payload":{"type":"sub_agent_complete","parentExecutionId":"exec-1","subSessionId":"` + subID + `",` + fields + `}}`
}

func kiroTestLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

// writeKiroTestSession writes a current-layout session with a sidecar and
// returns the path of its messages.jsonl.
func writeKiroTestSession(t *testing.T, root string, parentLines ...string) string {
	t.Helper()
	sessDir := filepath.Join(root, "workspace", kiroTestParentRawID)
	writeSourceFile(t, filepath.Join(sessDir, "session.json"),
		`{"title":"Parent session","workspacePaths":["/home/user/code/kiro-app"]}`)
	transcript := filepath.Join(sessDir, "messages.jsonl")
	writeSourceFile(t, transcript, kiroTestLines(parentLines...))
	return transcript
}

func kiroTestSubExecutionPath(transcript, subID string) string {
	return filepath.Join(filepath.Dir(transcript), "sub-executions", subID+".jsonl")
}

func parseKiroTestSessions(t *testing.T, root string) map[string]ParseResult {
	t.Helper()
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1, "a session and its subagents are one source")
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Empty(t, outcome.SourceErrors)
	require.NotEmpty(t, outcome.Results)
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
		`{"timestamp":"2026-06-01T10:00:00Z","payload":{"type":"user","content":"parent prompt"}}`,
		kiroTestSubAgentStart(kiroTestChildA, "coder", "do the subtask"),
		kiroTestSubAgentStart("../escape", "coder", "ignored spawn"),
		kiroTestSubAgentComplete(kiroTestChildA, `"status":"success","response":"subtask done"`),
		`{"timestamp":"2026-06-01T10:00:10Z","payload":{"type":"assistant","content":"parent reply"}}`,
	)
	writeSourceFile(t, kiroTestSubExecutionPath(transcript, kiroTestChildA), kiroTestLines(
		`{"timestamp":"2026-06-01T10:00:02Z","payload":{"type":"assistant","content":"child A working"}}`,
		kiroTestSubAgentStart(kiroTestChildB, "reviewer", "review the change"),
		kiroTestSubAgentComplete(kiroTestChildB, `"status":"error","response":"","errorMessage":"reviewer failed"`),
	))
	writeSourceFile(t, kiroTestSubExecutionPath(transcript, kiroTestChildB), kiroTestLines(
		`{"timestamp":"2026-06-01T10:00:03Z","payload":{"type":"assistant","content":"child B working"}}`,
	))
	writeSourceFile(t, kiroTestSubExecutionPath(transcript, kiroTestOrphan), kiroTestLines(
		`{"timestamp":"2026-06-01T10:00:04Z","payload":{"type":"user","content":"orphan prompt"}}`,
	))

	byID := parseKiroTestSessions(t, root)
	parentID := "kiro:" + kiroTestParentRawID
	require.Len(t, byID, 4)
	parent := byID[parentID]

	parentCalls := kiroTestToolCalls(parent)
	require.Len(t, parentCalls, 1, "a spawn with an invalid subagent session ID is ignored")
	spawn := parentCalls[kiroTestChildA]
	assert.Equal(t, "invoke_subagent", spawn.ToolName)
	assert.Equal(t, "Task", spawn.Category)
	assert.Equal(t, "kiro:"+kiroTestChildA, spawn.SubagentSessionID)
	assert.JSONEq(t,
		`{"subagent_type":"coder","description":"delegate the subtask","prompt":"do the subtask"}`,
		spawn.InputJSON)
	assert.Equal(t, map[string]string{kiroTestChildA: `"subtask done"`}, kiroTestToolResults(parent))

	tests := []struct {
		subID        string
		parentID     string
		sessionName  string
		firstMessage string
	}{
		{kiroTestChildA, parentID, "coder", "do the subtask"},
		{kiroTestChildB, "kiro:" + kiroTestChildA, "reviewer", "review the change"},
		{kiroTestOrphan, parentID, "", "orphan prompt"},
	}
	for _, tt := range tests {
		t.Run(tt.subID, func(t *testing.T) {
			child, ok := byID["kiro:"+tt.subID]
			require.True(t, ok, "subagent transcript should be emitted")
			assert.Equal(t, tt.parentID, child.Session.ParentSessionID)
			assert.Equal(t, RelSubagent, child.Session.RelationshipType)
			assert.Equal(t, kiroTestSubExecutionPath(transcript, tt.subID), child.Session.File.Path)
			assert.Equal(t, parent.Session.Project, child.Session.Project)
			assert.Equal(t, "/home/user/code/kiro-app", child.Session.Cwd)
			assert.Equal(t, "devbox", child.Session.Machine)
			assert.Equal(t, tt.sessionName, child.Session.SessionName)
			assert.Equal(t, tt.firstMessage, child.Session.FirstMessage)
		})
	}

	childA := byID["kiro:"+kiroTestChildA]
	assert.Equal(t, "kiro:"+kiroTestChildB, kiroTestToolCalls(childA)[kiroTestChildB].SubagentSessionID)
	assert.Equal(t, map[string]string{kiroTestChildB: `"reviewer failed"`}, kiroTestToolResults(childA),
		"a failed spawn reports its error message as the result")
}

func TestKiroProviderIgnoresSubExecutionLookalikes(t *testing.T) {
	root := t.TempDir()
	transcript := writeKiroTestSession(t, root,
		`{"payload":{"type":"user","content":"parent prompt"}}`,
	)
	sessDir := filepath.Dir(transcript)
	child := `{"payload":{"type":"assistant","content":"lookalike"}}` + "\n"
	writeSourceFile(t, kiroTestSubExecutionPath(transcript, kiroTestChildA), child)
	for _, path := range []string{
		filepath.Join(sessDir, "sub-executions", "nested", kiroTestChildB+".jsonl"),
		filepath.Join(sessDir, "sub-executions", "bad.id.jsonl"),
		filepath.Join(sessDir, "sub-executions", kiroTestOrphan+".json"),
		filepath.Join(sessDir, "snapshots", "sub-executions", kiroTestOrphan+".jsonl"),
		filepath.Join(root, "workspace", "sub-executions", kiroTestOrphan+".jsonl"),
	} {
		writeSourceFile(t, path, child)
	}

	byID := parseKiroTestSessions(t, root)
	assert.ElementsMatch(t,
		[]string{"kiro:" + kiroTestParentRawID, "kiro:" + kiroTestChildA},
		slices.Collect(maps.Keys(byID)))
}

func TestKiroProviderIgnoresSubExecutionSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), kiroTestChildA+".jsonl")
	writeSourceFile(t, outside, `{"payload":{"type":"assistant","content":"outside"}}`+"\n")
	transcript := writeKiroTestSession(t, root,
		`{"payload":{"type":"user","content":"parent prompt"}}`,
	)
	link := kiroTestSubExecutionPath(transcript, kiroTestChildA)
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	byID := parseKiroTestSessions(t, root)
	assert.Equal(t, []string{"kiro:" + kiroTestParentRawID}, slices.Collect(maps.Keys(byID)))
}

func TestKiroProviderFingerprintCoversSubExecutions(t *testing.T) {
	root := t.TempDir()
	transcriptText := `{"payload":{"type":"user","content":"parent prompt"}}` + "\n"
	transcript := writeKiroTestSession(t, root, strings.TrimSuffix(transcriptText, "\n"))
	sidecarInfo, err := os.Stat(filepath.Join(filepath.Dir(transcript), "session.json"))
	require.NoError(t, err)
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: kiroTestParentRawID})
	require.NoError(t, err)
	require.True(t, found)
	fingerprint := func() SourceFingerprint {
		t.Helper()
		fp, err := provider.Fingerprint(t.Context(), source)
		require.NoError(t, err)
		return fp
	}

	// Recorded from the fingerprint before subagent transcripts were
	// supported: sessions without them must keep it so they are not reparsed.
	base := fingerprint()
	assert.Equal(t, "998c34daf40686d851baf8ce51dc4b2df6fbc0c0660d3369e2ab4895fdc1ca98", base.Hash)
	assert.Equal(t, int64(len(transcriptText))+sidecarInfo.Size(), base.Size)

	writeSourceFile(t, filepath.Join(filepath.Dir(transcript), "sub-executions", "bad.id.jsonl"), "{}\n")
	assert.Equal(t, base, fingerprint(), "files that are not subagent transcripts are ignored")

	childPath := kiroTestSubExecutionPath(transcript, kiroTestChildA)
	childText := `{"payload":{"type":"assistant","content":"child v1"}}` + "\n"
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
}

func TestKiroProviderFindSourceResolvesSubagentToItsSession(t *testing.T) {
	root := t.TempDir()
	transcript := writeKiroTestSession(t, root,
		`{"payload":{"type":"user","content":"parent prompt"}}`,
	)
	childPath := kiroTestSubExecutionPath(transcript, kiroTestChildA)
	writeSourceFile(t, childPath, `{"payload":{"type":"assistant","content":"child"}}`+"\n")
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	tests := []struct {
		name string
		req  FindSourceRequest
		want bool
	}{
		{"raw subagent session ID", FindSourceRequest{RawSessionID: kiroTestChildA}, true},
		{"full subagent session ID", FindSourceRequest{FullSessionID: "kiro:" + kiroTestChildA}, true},
		{"stored subagent file path", FindSourceRequest{StoredFilePath: childPath}, true},
		{"unknown subagent session ID", FindSourceRequest{RawSessionID: kiroTestChildB}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, ok, err := provider.FindSource(t.Context(), tt.req)
			require.NoError(t, err)
			require.Equal(t, tt.want, ok)
			if tt.want {
				assert.Equal(t, transcript, found.DisplayPath,
					"a subagent resolves to the session source that parses it")
			}
		})
	}
}
