package sessionwatch

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type baselineStore struct {
	db.Store
	count atomic.Int64
}

func (s *baselineStore) GetSessionWatchState(id string) (db.SessionWatchState, error) {
	return db.SessionWatchState{State: db.SessionWatchResolved, PublicID: id}, nil
}

func (s *baselineStore) GetSessionVersion(context.Context, string) (int, int64, bool) {
	return int(s.count.Load()), 1, true
}

func TestEventsBaselineTakenBeforeReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		store := &baselineStore{}
		store.count.Store(1)
		updates := New(store, nil).Events(ctx, "session-a")
		store.count.Store(2)
		time.Sleep(PollInterval)
		synctest.Wait()
		select {
		case _, ok := <-updates:
			assert.True(t, ok)
		default:
			require.FailNow(t, "reply after watch creation was lost")
		}
	})
}

// testWatcher creates a Watcher backed by a fresh SQLite database
// and a minimal sync engine for tests that need checkDBForChanges
// access.
func testWatcher(t *testing.T) *Watcher {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {dir},
		},
		Machine: "test",
	})
	return New(database, engine)
}

func TestStatMtime_NonexistentFile(t *testing.T) {
	t.Parallel()
	got := StatMtime(
		filepath.Join(t.TempDir(), "no-such-file"),
	)
	assert.Equal(t, int64(0), got)
}

func TestStatMtime_ExistingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))
	got := StatMtime(path)
	assert.NotZero(t, got)
}

func TestCheckDBForChanges_FileDisappears(t *testing.T) {
	t.Parallel()
	w := testWatcher(t)

	path := filepath.Join(t.TempDir(), "gone.jsonl")
	var lastMtime int64 = 12345
	var mchanged time.Time
	var lastCount int
	var lastDBMtime int64

	changed := w.checkDBForChanges(t.Context(),
		"test-session",
		&lastCount,
		&lastDBMtime,
		&path,
		&lastMtime,
		&mchanged,
	)
	assert.False(t, changed, "expected no change signal")
	assert.Empty(t, path)
	assert.Equal(t, int64(0), lastMtime)
}

func TestCheckDBForChanges_FileHashChange(t *testing.T) {
	t.Parallel()
	w := testWatcher(t)
	database, ok := w.db.(*db.DB)
	require.True(t, ok, "test watcher should use SQLite DB")

	const sessionID = "hash-change"
	var mtime int64 = 12345
	hash1 := "shelley-fingerprint-1"
	dbtest.SeedSession(t, database, sessionID, "proj", func(s *db.Session) {
		s.MessageCount = 2
		s.FileMtime = &mtime
		s.FileHash = &hash1
	})

	lastCount, lastDBMtime, ok := w.db.GetSessionVersion(t.Context(), sessionID)
	require.True(t, ok, "initial session version")

	hash2 := "shelley-fingerprint-2"
	dbtest.SeedSession(t, database, sessionID, "proj", func(s *db.Session) {
		s.MessageCount = 2
		s.FileMtime = &mtime
		s.FileHash = &hash2
	})

	sourcePath := ""
	var lastFileMtime int64
	var mchanged time.Time
	changed := w.checkDBForChanges(t.Context(),
		sessionID,
		&lastCount,
		&lastDBMtime,
		&sourcePath,
		&lastFileMtime,
		&mchanged,
	)

	assert.True(t, changed,
		"file_hash-only rewrites must refresh session watchers")
}

func TestCheckDBForChangesPositAssistantSidecarAppendFallsBackToSync(
	t *testing.T,
) {
	root := t.TempDir()
	const conversationID = "11111111-1111-4111-8111-111111111111"
	conversationDir := filepath.Join(root, "workspace-1", conversationID)
	workspacePath := filepath.Join(root, "workspace-1", "workspace.json")
	conversationPath := filepath.Join(conversationDir, "conversation.json")
	lmMessagesPath := filepath.Join(conversationDir, "lm-messages.jsonl")
	usageEventsPath := filepath.Join(conversationDir, "usage-events.jsonl")

	dbtest.WriteTestFile(t, workspacePath, []byte(`{"path":"/work/example"}`))
	dbtest.WriteTestFile(t, conversationPath, []byte(`{
		"schemaVersion":"3",
		"root":{"id":"`+conversationID+`","timestamp":1735689600000,"metadata":{"kind":"main"}},
		"messages":[{"id":"node-1","parentId":"`+conversationID+`","isActive":true,"lmMessageIds":[0,1],"timestamp":1735689600000}],
		"files":[]
	}`))
	dbtest.WriteTestFile(t, lmMessagesPath, []byte(
		`{"id":0,"message":{"role":"user","content":"Inspect the project."}}`+"\n"+
			`{"id":1,"message":{"role":"assistant","content":[{"type":"text","text":"Looking."}],"providerOptions":{"providerMetadata":{"positai":{"timestamp":1735689601000,"modelId":"claude-sonnet-4-6","providerId":"anthropic","usage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":100}}}}}}`+"\n",
	))
	baseTime := time.Date(2026, time.August, 28, 10, 0, 0, 0, time.UTC)
	for _, path := range []string{workspacePath, conversationPath, lmMessagesPath} {
		require.NoError(t, os.Chtimes(path, baseTime, baseTime))
	}

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPositAssistant: {root},
		},
		Machine: "test",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	const sessionID = "posit-assistant:" + conversationID
	lastCount, lastDBVersion, ok := database.GetSessionVersion(t.Context(), sessionID)
	require.True(t, ok)
	sourcePath := engine.FindSourceFile(t.Context(), sessionID)
	require.Equal(t, conversationPath, sourcePath)
	lastFileMtime := engine.SourceMtime(t.Context(), sessionID)
	require.Equal(t, baseTime.UnixNano(), lastFileMtime)

	dbtest.WriteTestFile(t, usageEventsPath, []byte(
		`{"type":"usage","kind":"keepalive","timestamp":1735693200000,"anchorMessageId":"node-1","providerId":"anthropic","modelId":"claude-sonnet-4-6","inputTokens":2,"outputTokens":1,"totalTokens":24642,"cacheReadTokens":24639,"cacheWriteTokens":0}`+"\n",
	))
	sidecarTime := baseTime.Add(time.Minute)
	require.NoError(t, os.Chtimes(usageEventsPath, sidecarTime, sidecarTime))

	watcher := New(database, engine)
	var fileMtimeChangedAt time.Time
	changed := watcher.checkDBForChanges(t.Context(),
		sessionID,
		&lastCount,
		&lastDBVersion,
		&sourcePath,
		&lastFileMtime,
		&fileMtimeChangedAt,
	)
	assert.False(t, changed, "the fallback delay must elapse before direct sync")
	require.False(t, fileMtimeChangedAt.IsZero(),
		"the sidecar mtime must start the fallback timer")
	assert.Equal(t, sidecarTime.UnixNano(), lastFileMtime)

	fileMtimeChangedAt = time.Now().Add(-SyncFallbackDelay)
	changed = watcher.checkDBForChanges(t.Context(),
		sessionID,
		&lastCount,
		&lastDBVersion,
		&sourcePath,
		&lastFileMtime,
		&fileMtimeChangedAt,
	)
	require.True(t, changed, "the elapsed fallback must sync the sidecar")

	usageEvents, err := database.GetUsageEvents(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, usageEvents, 1)
	assert.Equal(t, "posit-assistant-keepalive", usageEvents[0].Source)
	assert.Equal(t, 24639, usageEvents[0].CacheReadInputTokens)
}

// kiroWatchedSession is a synced Kiro session with two subagents, watched
// through one of its session IDs, every one of which resolves to the
// session's messages.jsonl.
type kiroWatchedSession struct {
	database      *db.DB
	engine        *sync.Engine
	watcher       *Watcher
	sessionID     string
	sessDir       string
	transcript    string
	childPath     string
	siblingPath   string
	childLine     string
	baseTime      time.Time
	lastCount     int
	lastDBVersion int64
	sourcePath    string
	lastFileMtime int64
}

const (
	kiroWatchParentID = "kiro:sess_0123456789abcdef"
	kiroWatchChildID  = "kiro:11111111-2222-3333-4444-555555555555"
)

func watchKiroSession(t *testing.T, sessionID string) *kiroWatchedSession {
	t.Helper()
	root := t.TempDir()
	w := &kiroWatchedSession{
		sessionID: sessionID,
		sessDir:   filepath.Join(root, "workspace", "sess_0123456789abcdef"),
		childLine: testjsonl.KiroAssistantJSON("2026-06-01T10:00:02Z", "working"),
		baseTime:  time.Date(2026, time.June, 1, 10, 0, 5, 0, time.UTC),
	}
	transcriptPath := filepath.Join(w.sessDir, "messages.jsonl")
	w.transcript = transcriptPath
	sidecarPath := filepath.Join(w.sessDir, "session.json")
	w.childPath = filepath.Join(w.sessDir, "sub-executions", "11111111-2222-3333-4444-555555555555.jsonl")
	w.siblingPath = filepath.Join(w.sessDir, "sub-executions", "66666666-7777-8888-9999-000000000000.jsonl")
	dbtest.WriteTestFile(t, sidecarPath, []byte(`{"title":"Before"}`))
	dbtest.WriteTestFile(t, transcriptPath, []byte(testjsonl.JoinJSONL(
		testjsonl.KiroUserJSON("2026-06-01T10:00:00Z", "parent prompt"),
		testjsonl.KiroSubAgentStartJSON("invoke_subagent_toolu_test01-sub-agent-start", "2026-06-01T10:00:01Z",
			"11111111-2222-3333-4444-555555555555", "coder", "do the subtask"),
	)))
	dbtest.WriteTestFile(t, w.childPath, []byte(testjsonl.JoinJSONL(w.childLine)))
	dbtest.WriteTestFile(t, w.siblingPath, []byte(testjsonl.JoinJSONL(
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:03Z", "reviewing"),
	)))
	for _, path := range []string{sidecarPath, transcriptPath, w.childPath, w.siblingPath} {
		require.NoError(t, os.Chtimes(path, w.baseTime, w.baseTime))
	}

	w.database = dbtest.OpenTestDB(t)
	w.engine = sync.NewEngine(t.Context(), w.database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentKiro: {root}},
		Machine:   "test",
	})
	t.Cleanup(w.engine.Close)
	require.Equal(t, 3, w.engine.SyncAll(t.Context(), nil).Synced)
	w.watcher = New(w.database, w.engine)

	var ok bool
	w.lastCount, w.lastDBVersion, ok = w.database.GetSessionVersion(t.Context(), w.sessionID)
	require.True(t, ok)
	w.sourcePath = w.engine.FindSourceFile(t.Context(), w.sessionID)
	require.Equal(t, transcriptPath, w.sourcePath)
	w.lastFileMtime = w.engine.SourceMtime(t.Context(), w.sessionID)
	require.NotZero(t, w.lastFileMtime)
	return w
}

func (w *kiroWatchedSession) check(t *testing.T, fileMtimeChangedAt *time.Time) bool {
	t.Helper()
	return w.watcher.checkDBForChanges(t.Context(),
		w.sessionID,
		&w.lastCount,
		&w.lastDBVersion,
		&w.sourcePath,
		&w.lastFileMtime,
		fileMtimeChangedAt,
	)
}

// kiro-cli appends to a subagent's own transcript, not to the session's
// messages.jsonl. When the filesystem event for that append is missed, the
// fallback must still notice it and sync.
func TestCheckDBForChangesKiroSubagentAppendFallsBackToSync(t *testing.T) {
	w := watchKiroSession(t, kiroWatchChildID)
	dbtest.WriteTestFile(t, w.childPath, []byte(testjsonl.JoinJSONL(
		w.childLine, testjsonl.KiroAssistantJSON("2026-06-01T10:00:04Z", "subtask done"),
	)))
	childTime := w.baseTime.Add(time.Minute)
	require.NoError(t, os.Chtimes(w.childPath, childTime, childTime))

	var fileMtimeChangedAt time.Time
	assert.False(t, w.check(t, &fileMtimeChangedAt), "the fallback delay must elapse before direct sync")
	require.False(t, fileMtimeChangedAt.IsZero(), "a subagent-only append must start the fallback timer")

	fileMtimeChangedAt = time.Now().Add(-SyncFallbackDelay)
	require.True(t, w.check(t, &fileMtimeChangedAt), "the elapsed fallback must sync the subagent")

	messages, err := w.database.GetAllMessages(t.Context(), kiroWatchChildID)
	require.NoError(t, err)
	var contents []string
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	assert.Equal(t, []string{"working", "subtask done"}, contents)
}

// Every file a Kiro session source parses feeds the change token, so a
// missed event for any of them starts the fallback timer. None of these
// changes touches messages.jsonl or advances the newest mtime.
func TestCheckDBForChangesKiroSessionFilesStartFallback(t *testing.T) {
	tests := []struct {
		name   string
		watch  string
		change func(t *testing.T, w *kiroWatchedSession)
	}{
		{
			name:  "another subagent transcript appears",
			watch: kiroWatchChildID,
			change: func(t *testing.T, w *kiroWatchedSession) {
				t.Helper()
				path := filepath.Join(w.sessDir, "sub-executions", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl")
				dbtest.WriteTestFile(t, path, []byte(testjsonl.JoinJSONL(
					testjsonl.KiroAssistantJSON("2026-06-01T10:00:03Z", "testing"),
				)))
				require.NoError(t, os.Chtimes(path, w.baseTime, w.baseTime))
			},
		},
		{
			name:  "another subagent transcript is removed",
			watch: kiroWatchChildID,
			change: func(t *testing.T, w *kiroWatchedSession) {
				t.Helper()
				require.NoError(t, os.Remove(w.siblingPath))
			},
		},
		{
			name:  "the session metadata is rewritten",
			watch: kiroWatchChildID,
			change: func(t *testing.T, w *kiroWatchedSession) {
				t.Helper()
				path := filepath.Join(w.sessDir, "session.json")
				dbtest.WriteTestFile(t, path, []byte(`{"title":"After"}`))
				require.NoError(t, os.Chtimes(path, w.baseTime, w.baseTime))
			},
		},
		{
			name:  "the subagent directory becomes unreadable",
			watch: kiroWatchParentID,
			change: func(t *testing.T, w *kiroWatchedSession) {
				t.Helper()
				dir := filepath.Join(w.sessDir, "sub-executions")
				require.NoError(t, os.Chmod(dir, 0))
				t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, 0o755)) })
				if _, err := os.ReadDir(dir); err == nil {
					t.Skip("directory permissions do not block listing for this user")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := watchKiroSession(t, tt.watch)
			tt.change(t, w)

			var fileMtimeChangedAt time.Time
			assert.False(t, w.check(t, &fileMtimeChangedAt), "the fallback delay must elapse before direct sync")
			assert.False(t, fileMtimeChangedAt.IsZero(), "the change must start the fallback timer")
			assert.Equal(t, w.transcript, w.sourcePath, "the session must stay resolved to its transcript")
		})
	}
}
