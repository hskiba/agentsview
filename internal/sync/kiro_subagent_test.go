package sync_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// kiro-cli writes a session's transcript to
// <workspace>/sess_<id>/messages.jsonl and each subagent it spawns to
// <workspace>/sess_<id>/sub-executions/<subSessionId>.jsonl. The spawning
// transcript records the delegation as sub_agent_start/sub_agent_complete.
const (
	kiroSubagentParentRawID = "sess_0123456789abcdef"
	kiroSubagentParentID    = "kiro:" + kiroSubagentParentRawID
	kiroSubagentChildRawID  = "11111111-2222-3333-4444-555555555555"
	kiroSubagentChildID     = "kiro:" + kiroSubagentChildRawID
)

func writeKiroLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
}

func kiroSubAgentStart(id, ts, subID, name, prompt string) string {
	return `{"id":"invoke_subagent_` + id + `-sub-agent-start","timestamp":"` + ts + `","payload":{"type":"sub_agent_start","parentExecutionId":"exec-1","subSessionId":"` + subID + `","subAgentName":"` + name + `","prompt":"` + prompt + `","explanation":"delegate the subtask"}}`
}

func kiroSubAgentComplete(id, ts, subID, response string) string {
	return `{"id":"invoke_subagent_` + id + `-sub-agent-complete","timestamp":"` + ts + `","payload":{"type":"sub_agent_complete","parentExecutionId":"exec-1","subSessionId":"` + subID + `","status":"success","response":"` + response + `"}}`
}

// writeKiroSessionWithSubagent lays out one parent session that spawned one
// subagent and returns the session directory.
func writeKiroSessionWithSubagent(t *testing.T, kiroDir string) string {
	t.Helper()
	sessDir := filepath.Join(kiroDir, "workspace", kiroSubagentParentRawID)
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, "session.json"),
		[]byte(`{"title":"Parent session","workspacePaths":["/home/user/code/kiro-app"]}`),
		0o644,
	))
	writeKiroLines(t, filepath.Join(sessDir, "messages.jsonl"),
		`{"id":"u1","timestamp":"2026-06-01T10:00:00Z","payload":{"type":"user","content":"parent prompt"}}`,
		kiroSubAgentStart("toolu_test01", "2026-06-01T10:00:01Z", kiroSubagentChildRawID, "coder", "do the subtask"),
		kiroSubAgentComplete("toolu_test01", "2026-06-01T10:00:09Z", kiroSubagentChildRawID, "subtask done"),
		`{"id":"a1","timestamp":"2026-06-01T10:00:10Z","payload":{"type":"assistant","content":"parent reply"}}`,
	)
	writeKiroLines(t, filepath.Join(sessDir, "sub-executions", kiroSubagentChildRawID+".jsonl"),
		`{"id":"x1","timestamp":"2026-06-01T10:00:02Z","payload":{"type":"assistant","content":"SUBAGENT_MARKER working","subExecutionId":"`+kiroSubagentChildRawID+`"}}`,
		`{"id":"t1-call","timestamp":"2026-06-01T10:00:03Z","payload":{"type":"tool_call","toolCallId":"t1","toolName":"execute_bash","args":{"command":"ls"}}}`,
		`{"id":"t1-result","timestamp":"2026-06-01T10:00:04Z","payload":{"type":"tool_result","toolCallId":"t1","content":"ok"}}`,
	)
	return sessDir
}

// kiroSpawnLinks maps each Task tool call in a session to the subagent
// session it links to.
func kiroSpawnLinks(t *testing.T, database *db.DB, sessionID string) map[string]string {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	links := map[string]string{}
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.Category == "Task" {
				links[call.ToolName+":"+call.ToolUseID] = call.SubagentSessionID
			}
		}
	}
	return links
}

func TestKiroSubagentTranscriptSyncsFromChangedPath(t *testing.T) {
	const laterChildRawID = "66666666-7777-8888-9999-000000000000"
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)

	// kiro-cli spawns a second subagent after the first sync: the parent
	// gains a sub_agent_start and a new sub-executions file appears.
	transcript := filepath.Join(sessDir, "messages.jsonl")
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(kiroSubAgentStart("toolu_test02", "2026-06-01T10:01:00Z", laterChildRawID, "reviewer", "review it") + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	laterChild := filepath.Join(sessDir, "sub-executions", laterChildRawID+".jsonl")
	writeKiroLines(t, laterChild,
		`{"id":"y1","timestamp":"2026-06-01T10:01:01Z","payload":{"type":"assistant","content":"reviewing"}}`,
	)

	env.engine.SyncPaths([]string{laterChild})

	childIDs := func() []string {
		t.Helper()
		children, err := env.db.GetChildSessions(t.Context(), kiroSubagentParentID)
		require.NoError(t, err)
		ids := make([]string, len(children))
		for i, child := range children {
			assert.Nil(t, child.SourceMissingAt, "child %s should not be marked source-missing", child.ID)
			ids[i] = child.ID
		}
		return ids
	}
	want := []string{kiroSubagentChildID, "kiro:" + laterChildRawID}
	assert.ElementsMatch(t, want, childIDs(), "a watcher event for a new subagent transcript should sync it")

	stats = env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	assert.ElementsMatch(t, want, childIDs(), "a later full sync keeps every child")
}

func TestKiroSubagentTranscriptsSyncAsChildSessions(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	writeKiroSessionWithSubagent(t, env.kiroDir)

	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)

	parent, err := env.db.GetSessionFull(t.Context(), kiroSubagentParentID)
	require.NoError(t, err)
	require.NotNil(t, parent, "parent session must sync")

	children, err := env.db.GetChildSessions(t.Context(), kiroSubagentParentID)
	require.NoError(t, err)
	require.Len(t, children, 1, "the subagent transcript should sync as a child session")
	child := children[0]
	assert.Equal(t, kiroSubagentChildID, child.ID)
	assert.Equal(t, "subagent", child.RelationshipType)
	assert.Equal(t, parent.Project, child.Project)

	childMessages, err := env.db.GetAllMessages(t.Context(), kiroSubagentChildID)
	require.NoError(t, err)
	var childText []string
	for _, message := range childMessages {
		childText = append(childText, message.Content)
	}
	assert.Contains(t, strings.Join(childText, "\n"), "SUBAGENT_MARKER")

	assert.Equal(t,
		map[string]string{"invoke_subagent:" + kiroSubagentChildRawID: kiroSubagentChildID},
		kiroSpawnLinks(t, env.db, kiroSubagentParentID),
		"the parent transcript should record the spawn as a Task call linked to the child",
	)
}
