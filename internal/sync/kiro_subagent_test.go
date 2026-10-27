package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
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
	kiroSubagentLaterRawID  = "66666666-7777-8888-9999-000000000000"
	kiroSubagentLaterID     = "kiro:" + kiroSubagentLaterRawID
)

func writeKiroLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(lines...)), 0o644))
}

// appendKiroLine appends one record to a transcript, as kiro-cli does while a
// session runs.
func appendKiroLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open %s for append", path)
	defer func() { assert.NoError(t, f.Close(), "close %s", path) }()
	_, err = f.WriteString(line + "\n")
	require.NoError(t, err, "append to %s", path)
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
		testjsonl.KiroUserJSON("2026-06-01T10:00:00Z", "parent prompt"),
		testjsonl.KiroSubAgentStartJSON("invoke_subagent_toolu_test01-sub-agent-start", "2026-06-01T10:00:01Z",
			kiroSubagentChildRawID, "coder", "do the subtask"),
		testjsonl.KiroSubAgentCompleteJSON("invoke_subagent_toolu_test01-sub-agent-complete", "2026-06-01T10:00:09Z",
			kiroSubagentChildRawID, map[string]any{"status": "success", "response": "subtask done"}),
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:10Z", "parent reply"),
	)
	writeKiroLines(t, filepath.Join(sessDir, "sub-executions", kiroSubagentChildRawID+".jsonl"),
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:02Z", "SUBAGENT_MARKER working"),
		testjsonl.KiroToolCallJSON("2026-06-01T10:00:03Z", "t1", "execute_bash", map[string]any{"command": "ls"}),
		testjsonl.KiroToolResultJSON("2026-06-01T10:00:04Z", "t1", "ok"),
	)
	return sessDir
}

// spawnKiroLaterSubagent records a second spawn in the parent transcript and
// writes that subagent's transcript, returning its path.
func spawnKiroLaterSubagent(t *testing.T, sessDir string) string {
	t.Helper()
	appendKiroLine(t, filepath.Join(sessDir, "messages.jsonl"),
		testjsonl.KiroSubAgentStartJSON("invoke_subagent_toolu_test02-sub-agent-start", "2026-06-01T10:01:00Z",
			kiroSubagentLaterRawID, "reviewer", "review it"))
	laterChild := filepath.Join(sessDir, "sub-executions", kiroSubagentLaterRawID+".jsonl")
	writeKiroLines(t, laterChild, testjsonl.KiroAssistantJSON("2026-06-01T10:01:01Z", "reviewing"))
	return laterChild
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
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)

	// kiro-cli spawns a second subagent after the first sync: the parent
	// gains a sub_agent_start and a new sub-executions file appears.
	laterChild := spawnKiroLaterSubagent(t, sessDir)
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{laterChild}))

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
	want := []string{kiroSubagentChildID, kiroSubagentLaterID}
	assert.ElementsMatch(t, want, childIDs(), "a watcher event for a new subagent transcript should sync it")

	stats = env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	assert.ElementsMatch(t, want, childIDs(), "a later full sync keeps every child")

	// Removing a subagent transcript keeps its archived session and marks it
	// as missing its source.
	require.NoError(t, os.Remove(laterChild))
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{laterChild}))
	removed, err := env.db.GetSessionFull(t.Context(), kiroSubagentLaterID)
	require.NoError(t, err)
	require.NotNil(t, removed, "a removed subagent transcript must stay archived")
	assert.NotNil(t, removed.SourceMissingAt, "a removed subagent transcript should be marked source-missing")
	kept, err := env.db.GetSessionFull(t.Context(), kiroSubagentChildID)
	require.NoError(t, err)
	require.NotNil(t, kept)
	assert.Nil(t, kept.SourceMissingAt, "subagents whose transcripts remain stay current")
}

// writeKiroDecoySession writes an unrelated session with one subagent and
// returns that subagent's session ID.
func writeKiroDecoySession(t *testing.T, kiroDir string, i int) string {
	t.Helper()
	rawID := fmt.Sprintf("sess_%016x", i)
	childRawID := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
	sessDir := filepath.Join(kiroDir, "workspace", rawID)
	writeKiroLines(t, filepath.Join(sessDir, "messages.jsonl"),
		testjsonl.KiroUserJSON("2026-06-01T09:00:00Z", "decoy prompt"),
		testjsonl.KiroSubAgentStartJSON("invoke_subagent_toolu_decoy-sub-agent-start", "2026-06-01T09:00:01Z",
			childRawID, "coder", "decoy subtask"),
	)
	writeKiroLines(t, filepath.Join(sessDir, "sub-executions", childRawID+".jsonl"),
		testjsonl.KiroAssistantJSON("2026-06-01T09:00:02Z", "decoy work"),
	)
	return "kiro:" + childRawID
}

// kiroSubagentEventWork is what one archive did for an idle full sync and for
// the events that add and then remove one subagent transcript.
type kiroSubagentEventWork struct {
	idleSynced    int
	addTotal      int
	addSynced     int
	deleteTotal   int
	deleteSynced  int
	laterCaptured bool
	laterMissing  bool
	// affectedDecoys lists unrelated subagent sessions that the events
	// removed from the archive or marked source-missing.
	affectedDecoys []string
}

// measureKiroSubagentEventWork syncs an archive of one session with
// subagents plus decoys unrelated sessions, then adds and removes a subagent
// transcript through changed-path events.
func measureKiroSubagentEventWork(t *testing.T, decoys int) kiroSubagentEventWork {
	t.Helper()
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
	decoyChildren := make([]string, decoys)
	for i := range decoys {
		decoyChildren[i] = writeKiroDecoySession(t, env.kiroDir, i)
	}
	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "initial sync with %d decoys", decoys)
	idle := env.engine.SyncAll(t.Context(), nil)
	require.False(t, idle.Aborted, "idle sync with %d decoys", decoys)

	laterChild := spawnKiroLaterSubagent(t, sessDir)
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{laterChild}))
	addStats := env.engine.LastSyncStats()
	captured, err := env.db.GetSessionFull(t.Context(), kiroSubagentLaterID)
	require.NoError(t, err)

	require.NoError(t, os.Remove(laterChild))
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{laterChild}))
	deleteStats := env.engine.LastSyncStats()
	removed, err := env.db.GetSessionFull(t.Context(), kiroSubagentLaterID)
	require.NoError(t, err)
	require.NotNil(t, removed, "a removed subagent transcript must stay archived (%d decoys)", decoys)

	var affected []string
	for _, id := range decoyChildren {
		decoy, err := env.db.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		if decoy == nil || decoy.SourceMissingAt != nil {
			affected = append(affected, id)
		}
	}
	return kiroSubagentEventWork{
		idleSynced:     idle.Synced,
		addTotal:       addStats.TotalSessions,
		addSynced:      addStats.Synced,
		deleteTotal:    deleteStats.TotalSessions,
		deleteSynced:   deleteStats.Synced,
		laterCaptured:  captured != nil,
		laterMissing:   removed.SourceMissingAt != nil,
		affectedDecoys: affected,
	}
}

// A subagent event touches only its own session: an archive of unrelated
// sessions with subagents must not change how much work the event does,
// for new transcripts or for deleted ones.
func TestKiroSubagentChangedPathWorkIsCardinalityBounded(t *testing.T) {
	small := measureKiroSubagentEventWork(t, 3)
	assert.Zero(t, small.idleSynced, "an unchanged full sync should not rewrite sessions or subagents")
	assert.True(t, small.laterCaptured, "a watcher event for a new subagent transcript should sync it")
	assert.True(t, small.laterMissing, "a removed subagent transcript should be marked source-missing")
	assert.Empty(t, small.affectedDecoys, "unrelated sessions and subagents stay archived and current")

	large := measureKiroSubagentEventWork(t, 30)
	assert.Equal(t, small, large,
		"subagent changed-path work, deletion, and idle syncs must be the same with 3 and 30 unrelated sessions")
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

// A session and its subagents are written as one source. When a refresh
// writes the session but fails to write a subagent, the session must stay
// stale so the next ordinary sync retries the subagent instead of skipping
// the unchanged source.
func TestKiroSubagentWriteFailureRetriesWholeSession(t *testing.T) {
	tests := []struct {
		name    string
		refresh func(t *testing.T, env *testEnv)
	}{
		{
			name: "single-session refresh",
			refresh: func(t *testing.T, env *testEnv) {
				t.Helper()
				require.Error(t, env.engine.SyncSingleSession(kiroSubagentParentID))
			},
		},
		{
			name: "full sync",
			refresh: func(t *testing.T, env *testEnv) {
				t.Helper()
				stats := env.engine.SyncAll(t.Context(), nil)
				require.False(t, stats.Aborted)
				require.Equal(t, 1, stats.Failed)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentKiro)
			sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
			stats := env.engine.SyncAll(t.Context(), nil)
			require.False(t, stats.Aborted)
			require.Zero(t, stats.Failed)

			spawnKiroLaterSubagent(t, sessDir)
			raw, err := sql.Open("sqlite3", env.db.Path())
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, raw.Close()) })
			_, err = raw.ExecContext(t.Context(), `
				CREATE TRIGGER fail_kiro_subagent_insert
				BEFORE INSERT ON sessions
				WHEN NEW.id = '`+kiroSubagentLaterID+`'
				BEGIN
					SELECT RAISE(FAIL, 'injected Kiro subagent write failure');
				END;
			`)
			require.NoError(t, err)

			tt.refresh(t, env)
			later, err := env.db.GetSessionFull(t.Context(), kiroSubagentLaterID)
			require.NoError(t, err)
			require.Nil(t, later, "the injected failure must leave the new subagent unwritten")

			_, err = raw.ExecContext(t.Context(), `DROP TRIGGER fail_kiro_subagent_insert`)
			require.NoError(t, err)
			retry := env.engine.SyncAll(t.Context(), nil)
			require.False(t, retry.Aborted)
			assert.Zero(t, retry.Failed)

			later, err = env.db.GetSessionFull(t.Context(), kiroSubagentLaterID)
			require.NoError(t, err)
			assert.NotNil(t, later, "the next sync must retry the subagent the refresh failed to write")
			parent, err := env.db.GetSessionFull(t.Context(), kiroSubagentParentID)
			require.NoError(t, err)
			require.NotNil(t, parent)
			assert.Equal(t, db.CurrentDataVersion(), parent.DataVersion,
				"the session is current once every subagent is written")
		})
	}
}

// kiroSessionText joins the content of a session's stored messages.
func kiroSessionText(t *testing.T, database *db.DB, sessionID string) string {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	text := make([]string, len(messages))
	for i, message := range messages {
		text[i] = message.Content
	}
	return strings.Join(text, "\n")
}

// An unreadable subagent transcript must not hold back its session or the
// other subagents; the session stays stale until it can be read.
func TestKiroUnreadableSubagentLeavesSessionAndSiblingsSyncing(t *testing.T) {
	breaks := []struct {
		name       string
		breakChild func(t *testing.T, path string) (restore func())
	}{
		{
			name: "permission denied",
			breakChild: func(t *testing.T, path string) func() {
				t.Helper()
				require.NoError(t, os.Chmod(path, 0))
				t.Cleanup(func() { assert.NoError(t, os.Chmod(path, 0o644)) })
				if _, err := os.ReadFile(path); err == nil {
					t.Skip("file permissions do not block reads for this user")
				}
				return func() { require.NoError(t, os.Chmod(path, 0o644)) }
			},
		},
		{
			name: "symlink loop",
			breakChild: func(t *testing.T, path string) func() {
				t.Helper()
				content, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.Remove(path))
				if err := os.Symlink(path, path); err != nil {
					t.Skipf("symlink creation unavailable: %v", err)
				}
				return func() {
					require.NoError(t, os.Remove(path))
					require.NoError(t, os.WriteFile(path, content, 0o644))
				}
			},
		},
	}
	refreshes := []struct {
		name    string
		refresh func(t *testing.T, env *testEnv, changed string)
	}{
		{
			name: "full sync",
			refresh: func(t *testing.T, env *testEnv, _ string) {
				t.Helper()
				stats := env.engine.SyncAll(t.Context(), nil)
				require.False(t, stats.Aborted)
				assert.Equal(t, 1, stats.Failed, "the unreadable subagent is reported")
			},
		},
		{
			name: "changed path",
			refresh: func(t *testing.T, env *testEnv, changed string) {
				t.Helper()
				assert.Error(t, env.engine.SyncPathsContext(t.Context(), []string{changed}),
					"the unreadable subagent leaves the refresh incomplete")
			},
		},
		{
			name: "single-session refresh",
			refresh: func(t *testing.T, env *testEnv, _ string) {
				t.Helper()
				assert.NoError(t, env.engine.SyncSingleSession(kiroSubagentParentID),
					"the requested session refreshes")
			},
		},
	}
	for _, br := range breaks {
		for _, rf := range refreshes {
			t.Run(br.name+"/"+rf.name, func(t *testing.T) {
				env := setupSingleAgentTestEnv(t, parser.AgentKiro)
				sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
				laterChild := spawnKiroLaterSubagent(t, sessDir)
				stats := env.engine.SyncAll(t.Context(), nil)
				require.False(t, stats.Aborted)
				require.Zero(t, stats.Failed)

				appendKiroLine(t, filepath.Join(sessDir, "messages.jsonl"),
					testjsonl.KiroAssistantJSON("2026-06-01T10:02:00Z", "PARENT_UPDATE"))
				sibling := filepath.Join(sessDir, "sub-executions", kiroSubagentChildRawID+".jsonl")
				appendKiroLine(t, sibling,
					testjsonl.KiroAssistantJSON("2026-06-01T10:02:01Z", "SIBLING_UPDATE"))
				restore := br.breakChild(t, laterChild)

				rf.refresh(t, env, sibling)
				assert.Contains(t, kiroSessionText(t, env.db, kiroSubagentParentID), "PARENT_UPDATE",
					"the session syncs past its unreadable subagent")
				assert.Contains(t, kiroSessionText(t, env.db, kiroSubagentChildID), "SIBLING_UPDATE",
					"a readable subagent syncs past its unreadable sibling")
				later, err := env.db.GetSessionFull(t.Context(), kiroSubagentLaterID)
				require.NoError(t, err)
				require.NotNil(t, later, "the unreadable subagent stays archived")
				assert.Nil(t, later.SourceMissingAt, "an unreadable subagent transcript is not missing")
				parent, err := env.db.GetSessionFull(t.Context(), kiroSubagentParentID)
				require.NoError(t, err)
				require.NotNil(t, parent)
				assert.Less(t, parent.DataVersion, db.CurrentDataVersion(),
					"the session stays stale so the next sync retries the unreadable subagent")

				restore()
				appendKiroLine(t, laterChild,
					testjsonl.KiroAssistantJSON("2026-06-01T10:02:02Z", "LATER_UPDATE"))
				retry := env.engine.SyncAll(t.Context(), nil)
				require.False(t, retry.Aborted)
				assert.Zero(t, retry.Failed)
				assert.Contains(t, kiroSessionText(t, env.db, kiroSubagentLaterID), "LATER_UPDATE",
					"the subagent syncs once it can be read")
				parent, err = env.db.GetSessionFull(t.Context(), kiroSubagentParentID)
				require.NoError(t, err)
				require.NotNil(t, parent)
				assert.Equal(t, db.CurrentDataVersion(), parent.DataVersion,
					"the session is current once every subagent is written")
			})
		}
	}
}

// A subagent spawned by an unreadable subagent has no readable spawn record,
// so its archived prompt must survive until the spawner can be read again.
func TestKiroNestedSubagentKeepsSpawnWhileSpawnerUnreadable(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
	spawner := filepath.Join(sessDir, "sub-executions", kiroSubagentChildRawID+".jsonl")
	appendKiroLine(t, spawner, testjsonl.KiroSubAgentStartJSON("invoke_subagent_toolu_test03-sub-agent-start",
		"2026-06-01T10:00:05Z", kiroSubagentLaterRawID, "nested", "NESTED_PROMPT"))
	nested := filepath.Join(sessDir, "sub-executions", kiroSubagentLaterRawID+".jsonl")
	writeKiroLines(t, nested, testjsonl.KiroAssistantJSON("2026-06-01T10:00:06Z", "nested working"))
	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	require.Zero(t, stats.Failed)

	require.NoError(t, os.Chmod(spawner, 0))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(spawner, 0o644)) })
	if _, err := os.ReadFile(spawner); err == nil {
		t.Skip("file permissions do not block reads for this user")
	}
	appendKiroLine(t, nested, testjsonl.KiroAssistantJSON("2026-06-01T10:00:07Z", "nested update"))
	stats = env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)

	assert.Contains(t, kiroSessionText(t, env.db, kiroSubagentLaterID), "NESTED_PROMPT",
		"the nested subagent keeps the prompt its spawner delivered")
}

// Only a session transcript's rows are completed together. A Kiro SQLite
// store holds independent sessions, so a failed write leaves the others
// current.
func TestKiroSQLiteWriteFailureLeavesOtherSessionsCurrent(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	ks := createKiroSQLiteDB(t, env.kiroDir)
	payload := readKiroSQLiteFixture(t, "standard_payload.json")
	for _, id := range []string{"healthy-session", "failing-session"} {
		ks.addSession(t, "/home/user/code/kiro-app", id, payload, 1779012000000, 1779012030000)
	}
	raw, err := sql.Open("sqlite3", env.db.Path())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, raw.Close()) })
	_, err = raw.ExecContext(t.Context(), `
		CREATE TRIGGER fail_kiro_sqlite_insert
		BEFORE INSERT ON sessions
		WHEN NEW.id = 'kiro:failing-session'
		BEGIN
			SELECT RAISE(FAIL, 'injected Kiro SQLite write failure');
		END;
	`)
	require.NoError(t, err)

	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	require.Equal(t, 1, stats.Failed)

	healthy, err := env.db.GetSessionFull(t.Context(), "kiro:healthy-session")
	require.NoError(t, err)
	require.NotNil(t, healthy)
	assert.Equal(t, db.CurrentDataVersion(), healthy.DataVersion)
}
