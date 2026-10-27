package parser

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Kiro JSONL message kinds.
const (
	kiroKindPrompt    = "Prompt"
	kiroKindAssistant = "AssistantMessage"
	kiroKindToolRes   = "ToolResults"
)

// kiroMeta holds fields from the companion .json metadata file.
type kiroMeta struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// discoverLegacyJSONL finds all .jsonl session files under a Kiro
// CLI sessions directory. Layout:
// <sessionsDir>/<uuid>.jsonl  (with companion <uuid>.json)
func (s kiroSourceSet) discoverLegacyJSONL(sessionsDir string) ([]DiscoveredFile, error) {
	var files []DiscoveredFile
	for _, dir := range []string{sessionsDir, filepath.Join(sessionsDir, "cli")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read Kiro legacy directory %s: %w", dir, err)
			}
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if _, err := loadKiroMetaStrict(path); err != nil {
				return nil, err
			}
			regular, err := kiroRegularFileUnderRootChecked(sessionsDir, path)
			if err != nil {
				return nil, fmt.Errorf("probe Kiro legacy transcript %s: %w", path, err)
			}
			if regular {
				files = append(files, DiscoveredFile{Path: path, Agent: AgentKiro})
			}
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

// legacySourceFile locates a legacy Kiro JSONL session file by its raw
// session ID (without the "kiro:" prefix).
func (s kiroSourceSet) legacySourceFile(sessionsDir, rawID string) string {
	if sessionsDir == "" || !IsValidSessionID(rawID) {
		return ""
	}
	for _, dir := range []string{sessionsDir, filepath.Join(sessionsDir, "cli")} {
		candidate := filepath.Join(dir, rawID+".jsonl")
		if _, err := os.Stat(candidate); err == nil && kiroRegularFileUnderRoot(sessionsDir, candidate) {
			return candidate
		}
	}
	return ""
}

// KiroSessionIDFromPath returns the logical raw session ID for a
// legacy JSONL-backed Kiro session, preferring companion metadata
// when present because the filename is only a storage detail.
func KiroSessionIDFromPath(jsonlPath string) string {
	if meta := loadKiroMeta(jsonlPath); meta != nil &&
		meta.SessionID != "" {
		return meta.SessionID
	}
	return strings.TrimSuffix(filepath.Base(jsonlPath), ".jsonl")
}

// loadKiroMeta reads the companion .json metadata file for a
// session JSONL file.
func loadKiroMeta(jsonlPath string) *kiroMeta {
	meta, _ := loadKiroMetaStrict(jsonlPath)
	return meta
}

func loadKiroMetaStrict(jsonlPath string) (*kiroMeta, error) {
	jsonPath := strings.TrimSuffix(jsonlPath, ".jsonl") + ".json"
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read Kiro legacy metadata %s: %w", jsonPath, err)
	}
	var m kiroMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode Kiro legacy metadata %s: %w", jsonPath, err)
	}
	return &m, nil
}

// parseLegacySessionContext parses a Kiro CLI session from its JSONL file.
// Returns (nil, nil, nil) if the file doesn't exist or contains
// no user/assistant messages.
func (p *kiroProvider) parseLegacySessionContext(
	ctx context.Context, path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
	var messages []ParsedMessage
	var firstMessage string
	ordinal := 0

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}

		kind := gjson.Get(line, "kind").Str
		data := gjson.Get(line, "data")

		switch kind {
		case kiroKindPrompt:
			content := kiroExtractText(data)
			if content == "" {
				continue
			}
			if firstMessage == "" {
				firstMessage = truncate(
					strings.ReplaceAll(content, "\n", " "), 300,
				)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       content,
				ContentLength: len(content),
			})
			ordinal++

		case kiroKindAssistant:
			text, toolCalls := kiroExtractAssistant(data)
			hasToolUse := len(toolCalls) > 0

			displayContent := text
			if hasToolUse && text == "" {
				displayContent = kiroFormatToolCalls(toolCalls)
			}
			if displayContent == "" && !hasToolUse {
				continue
			}

			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       displayContent,
				ContentLength: len(displayContent),
				HasToolUse:    hasToolUse,
				ToolCalls:     toolCalls,
			})
			ordinal++

		case kiroKindToolRes:
			results := kiroExtractToolResults(data)
			if len(results) == 0 {
				continue
			}
			messages = append(messages, ParsedMessage{
				Ordinal:     ordinal,
				Role:        RoleUser,
				ToolResults: results,
			})
			ordinal++
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil,
			fmt.Errorf("reading kiro %s: %w", path, err)
	}

	// Require at least one message with content.
	hasContent := false
	for _, m := range messages {
		if m.Content != "" {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return nil, nil, nil
	}

	// Extract metadata from companion .json file.
	meta, err := loadKiroMetaStrict(path)
	if err != nil {
		return nil, nil, err
	}

	sessionID := strings.TrimSuffix(
		filepath.Base(path), ".jsonl",
	)

	var project, cwd, sessionName string
	var startedAt, endedAt time.Time

	if meta != nil {
		if meta.SessionID != "" {
			sessionID = meta.SessionID
		}
		cwd = meta.Cwd
		if cwd != "" {
			project = ExtractProjectFromCwdWithBranchContext(ctx, cwd, "")
		}
		sessionName = strings.TrimSpace(meta.Title)
		if firstMessage == "" {
			firstMessage = sessionName
		}
		startedAt = parseTimestamp(meta.CreatedAt)
		endedAt = parseTimestamp(meta.UpdatedAt)
	}

	if project == "" {
		project = "unknown"
	}

	sessionID = "kiro:" + sessionID

	userCount := 0
	for _, m := range messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentKiro,
		Cwd:              cwd,
		FirstMessage:     firstMessage,
		SessionName:      sessionName,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	return sess, messages, nil
}

type kiroCurrentMeta struct {
	Title          string   `json:"title"`
	CreatedAt      string   `json:"createdAt"`
	LastModifiedAt string   `json:"lastModifiedAt"`
	WorkspacePaths []string `json:"workspacePaths"`
}

// kiroSubagentToolName is the Task tool name recorded for a kiro-cli
// subagent spawn. kiro-cli writes the spawn as sub_agent_start and
// sub_agent_complete records instead of a tool_call/tool_result pair.
const kiroSubagentToolName = "invoke_subagent"

// kiroSubagentSpawn is a sub_agent_start record found in a Kiro transcript.
type kiroSubagentSpawn struct {
	SubSessionID string
	Name         string
	Prompt       string
	Timestamp    time.Time
}

// isKiroSubagentID reports whether id can name a subagent session. A
// subagent is stored as kiro:<id> beside top-level sessions, so an ID shaped
// like a session directory (sess_<id>) is rejected: it would collide with
// that session.
func isKiroSubagentID(id string) bool {
	return IsValidSessionID(id) && !isKiroCurrentSessionDir(id)
}

// kiroSubagentInput is the Task tool input recorded for a subagent spawn,
// using the field names the session viewer reads for Task calls.
type kiroSubagentInput struct {
	SubagentType string `json:"subagent_type,omitempty"`
	Description  string `json:"description,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
}

// kiroCurrentTranscript is one parsed current-layout Kiro JSONL file: either
// a session's messages.jsonl or one of its sub-executions transcripts.
type kiroCurrentTranscript struct {
	Messages        []ParsedMessage
	FirstMessage    string
	EarliestMessage time.Time
	LatestMessage   time.Time
	Spawns          []kiroSubagentSpawn
	ModTime         time.Time
	Size            int64
}

func (t kiroCurrentTranscript) userMessageCount() int {
	count := 0
	for _, msg := range t.Messages {
		if msg.Role == RoleUser && msg.Content != "" {
			count++
		}
	}
	return count
}

// bounds returns the first and last accepted message timestamps, falling back
// to the file's modification time when no record carried a timestamp.
func (t kiroCurrentTranscript) bounds() (time.Time, time.Time) {
	startedAt, endedAt := t.EarliestMessage, t.LatestMessage
	if startedAt.IsZero() {
		startedAt = t.ModTime
	}
	if endedAt.IsZero() {
		endedAt = t.ModTime
	}
	return startedAt, endedAt
}

// readKiroCurrentTranscript parses one current-layout Kiro JSONL file. A
// missing file parses to a transcript with no messages.
func readKiroCurrentTranscript(path string) (kiroCurrentTranscript, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return kiroCurrentTranscript{}, nil
		}
		return kiroCurrentTranscript{}, fmt.Errorf("stat %s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return kiroCurrentTranscript{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
	var messages []ParsedMessage
	var spawns []kiroSubagentSpawn
	firstMessage := ""
	ordinal := 0
	var earliestMessage, latestMessage time.Time
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}
		payload := gjson.Get(line, "payload")
		timestamp := kiroCurrentMessageTimestamp(gjson.Get(line, "timestamp"))
		typ := payload.Get("type").Str
		content := strings.TrimSpace(payload.Get("content").Str)
		accepted := false
		switch typ {
		case "user", "assistant":
			if content == "" {
				continue
			}
			role := RoleUser
			if typ == "assistant" {
				role = RoleAssistant
			}
			if role == RoleUser && firstMessage == "" {
				firstMessage = truncate(strings.ReplaceAll(content, "\n", " "), 300)
			}
			messages = append(messages, ParsedMessage{Ordinal: ordinal, Role: role, Content: content, Timestamp: timestamp, ContentLength: len(content), Model: payload.Get("reasoningModelId").Str})
			accepted = true
			ordinal++
		case "tool_call":
			name, id := payload.Get("toolName").Str, payload.Get("toolCallId").Str
			if name == "" || id == "" {
				continue
			}
			call := ParsedToolCall{ToolUseID: id, ToolName: name, Category: NormalizeToolCategory(name), InputJSON: payload.Get("args").Raw}
			display := kiroFormatToolCalls([]ParsedToolCall{call})
			messages = append(messages, ParsedMessage{Ordinal: ordinal, Role: RoleAssistant, Content: display, Timestamp: timestamp, ContentLength: len(display), HasToolUse: true, ToolCalls: []ParsedToolCall{call}})
			accepted = true
			ordinal++
		case "tool_result":
			id := payload.Get("toolCallId").Str
			if id == "" {
				continue
			}
			raw := payload.Get("content").Raw
			messages = append(messages, ParsedMessage{Ordinal: ordinal, Role: RoleUser, Timestamp: timestamp, ToolResults: []ParsedToolResult{{ToolUseID: id, ContentRaw: raw, ContentLength: len(raw)}}})
			accepted = true
			ordinal++
		case "sub_agent_start":
			// Record ids repeat in real kiro-cli output, so the subagent's
			// session ID is the only reliable key pairing start, complete,
			// and the subagent's own transcript.
			subID := payload.Get("subSessionId").Str
			if !isKiroSubagentID(subID) {
				continue
			}
			spawn := kiroSubagentSpawn{SubSessionID: subID, Name: payload.Get("subAgentName").Str, Prompt: payload.Get("prompt").Str, Timestamp: timestamp}
			input, err := json.Marshal(kiroSubagentInput{SubagentType: spawn.Name, Description: payload.Get("explanation").Str, Prompt: spawn.Prompt})
			if err != nil {
				return kiroCurrentTranscript{}, fmt.Errorf("encode Kiro subagent input in %s: %w", path, err)
			}
			call := ParsedToolCall{ToolUseID: subID, ToolName: kiroSubagentToolName, Category: NormalizeToolCategory(kiroSubagentToolName), InputJSON: string(input), SubagentSessionID: "kiro:" + subID}
			display := kiroFormatToolCalls([]ParsedToolCall{call})
			messages = append(messages, ParsedMessage{Ordinal: ordinal, Role: RoleAssistant, Content: display, Timestamp: timestamp, ContentLength: len(display), HasToolUse: true, ToolCalls: []ParsedToolCall{call}})
			spawns = append(spawns, spawn)
			accepted = true
			ordinal++
		case "sub_agent_complete":
			subID := payload.Get("subSessionId").Str
			if !isKiroSubagentID(subID) {
				continue
			}
			// A failed spawn reports an errorMessage and may leave response
			// empty; any non-empty response is kept as the result.
			response := payload.Get("response")
			raw := response.Raw
			if !response.Exists() || response.Type == gjson.Null || (response.Type == gjson.String && response.Str == "") {
				if errRaw := payload.Get("errorMessage").Raw; errRaw != "" {
					raw = errRaw
				}
			}
			messages = append(messages, ParsedMessage{Ordinal: ordinal, Role: RoleUser, Timestamp: timestamp, ToolResults: []ParsedToolResult{{ToolUseID: subID, ContentRaw: raw, ContentLength: len(raw)}}})
			accepted = true
			ordinal++
		}
		if accepted && !timestamp.IsZero() {
			if earliestMessage.IsZero() || timestamp.Before(earliestMessage) {
				earliestMessage = timestamp
			}
			if latestMessage.IsZero() || timestamp.After(latestMessage) {
				latestMessage = timestamp
			}
		}
	}
	if err := lr.Err(); err != nil {
		return kiroCurrentTranscript{}, fmt.Errorf("reading kiro %s: %w", path, err)
	}
	return kiroCurrentTranscript{
		Messages:        messages,
		FirstMessage:    firstMessage,
		EarliestMessage: earliestMessage,
		LatestMessage:   latestMessage,
		Spawns:          spawns,
		ModTime:         info.ModTime(),
		Size:            info.Size(),
	}, nil
}

// parseCurrentSession parses a session's messages.jsonl and returns the
// session with its transcript, including the subagent spawns it recorded.
func (p *kiroProvider) parseCurrentSession(
	ctx context.Context, path, sessionID, machine string,
) (*ParsedSession, kiroCurrentTranscript, error) {
	transcript, err := readKiroCurrentTranscript(path)
	if err != nil {
		return nil, kiroCurrentTranscript{}, err
	}
	if len(transcript.Messages) == 0 {
		return nil, kiroCurrentTranscript{}, nil
	}
	messages := transcript.Messages
	meta := kiroCurrentMeta{}
	if sidecar, ok := kiroCurrentSidecarPath(p.sources.roots, path); ok {
		data, readErr := os.ReadFile(sidecar)
		if readErr != nil {
			return nil, kiroCurrentTranscript{}, fmt.Errorf("read Kiro current metadata %s: %w", sidecar, readErr)
		}
		if unmarshalErr := json.Unmarshal(data, &meta); unmarshalErr != nil {
			return nil, kiroCurrentTranscript{}, fmt.Errorf("decode Kiro current metadata %s: %w", sidecar, unmarshalErr)
		}
	}
	cwd := ""
	if len(meta.WorkspacePaths) > 0 {
		cwd = meta.WorkspacePaths[0]
	}
	project := ExtractProjectFromCwdWithBranchContext(ctx, cwd, "")
	if project == "" {
		project = "unknown"
	}
	startedAt, endedAt := parseTimestamp(meta.CreatedAt), parseTimestamp(meta.LastModifiedAt)
	firstAt, lastAt := transcript.bounds()
	if startedAt.IsZero() {
		startedAt = firstAt
	}
	if endedAt.IsZero() {
		endedAt = lastAt
	}
	return &ParsedSession{ID: "kiro:" + sessionID, Project: project, Machine: machine, Agent: AgentKiro, Cwd: cwd, FirstMessage: transcript.FirstMessage, StartedAt: startedAt, EndedAt: endedAt, MessageCount: len(messages), UserMessageCount: transcript.userMessageCount(), SessionName: meta.Title, File: FileInfo{Path: path, Size: transcript.Size, Mtime: transcript.ModTime.UnixNano()}}, transcript, nil
}

// kiroSubExecutionsDirName is the directory inside a current-layout session
// directory where kiro-cli writes one transcript per spawned subagent,
// including subagents spawned by other subagents.
const kiroSubExecutionsDirName = "sub-executions"

// kiroSubExecutionFiles lists the subagent transcripts admitted for a
// session transcript: regular <subSessionId>.jsonl files directly inside the
// session's sub-executions directory that resolve inside root, sorted by
// name. Subdirectories, other extensions, invalid or session-shaped IDs, and
// symlinks that escape root are ignored.
func kiroSubExecutionFiles(root, transcript string) ([]string, error) {
	dir := filepath.Join(filepath.Dir(transcript), kiroSubExecutionsDirName)
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat Kiro sub-executions %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read Kiro sub-executions %s: %w", dir, err)
	}
	var files []string
	for _, entry := range entries {
		subID, ok := strings.CutSuffix(entry.Name(), ".jsonl")
		if !ok || !isKiroSubagentID(subID) || entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		regular, err := kiroRegularFileUnderRootChecked(root, path)
		if err != nil {
			return nil, fmt.Errorf("probe Kiro sub-execution %s: %w", path, err)
		}
		if regular {
			files = append(files, path)
		}
	}
	slices.Sort(files)
	return files, nil
}

// parseCurrentSubExecutions parses the subagent transcripts of a session as
// child sessions. Each child links to the transcript whose sub_agent_start
// names it, which is a sibling subagent for nested spawns, and inherits the
// parent session's project, working directory, and machine. A child file
// that cannot be read becomes a retryable per-session error. Cancelling ctx
// stops the parse between subagent files.
func parseCurrentSubExecutions(
	ctx context.Context, root string, parent *ParsedSession, parentSpawns []kiroSubagentSpawn,
) ([]ParseResult, []SourceError, error) {
	files, err := kiroSubExecutionFiles(root, parent.File.Path)
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, nil
	}
	type spawnOrigin struct {
		spawn    kiroSubagentSpawn
		parentID string
	}
	origins := make(map[string]spawnOrigin)
	recordFirstSpawner := func(spawnerID string, spawns []kiroSubagentSpawn) {
		for _, spawn := range spawns {
			if _, seen := origins[spawn.SubSessionID]; !seen {
				origins[spawn.SubSessionID] = spawnOrigin{spawn: spawn, parentID: spawnerID}
			}
		}
	}
	recordFirstSpawner(parent.ID, parentSpawns)
	type childTranscript struct {
		path       string
		subID      string
		transcript kiroCurrentTranscript
	}
	var parsed []childTranscript
	var sourceErrs []SourceError
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		subID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		transcript, err := readKiroCurrentTranscript(path)
		if err != nil {
			sourceErrs = append(sourceErrs, SourceError{SourceKey: path, DisplayPath: path, SessionID: "kiro:" + subID, Err: err, Retryable: true})
			continue
		}
		if len(transcript.Messages) == 0 {
			continue
		}
		recordFirstSpawner("kiro:"+subID, transcript.Spawns)
		parsed = append(parsed, childTranscript{path: path, subID: subID, transcript: transcript})
	}
	results := make([]ParseResult, 0, len(parsed))
	for _, child := range parsed {
		origin, known := origins[child.subID]
		parentID := parent.ID
		if known && origin.parentID != "kiro:"+child.subID {
			parentID = origin.parentID
		}
		transcript := child.transcript
		if transcript.userMessageCount() == 0 {
			transcript = transcript.withSpawnPrompt(origin.spawn)
		}
		startedAt, endedAt := transcript.bounds()
		results = append(results, ParseResult{
			Session: ParsedSession{
				ID:               "kiro:" + child.subID,
				Project:          parent.Project,
				Machine:          parent.Machine,
				Agent:            AgentKiro,
				Cwd:              parent.Cwd,
				FirstMessage:     transcript.FirstMessage,
				StartedAt:        startedAt,
				EndedAt:          endedAt,
				MessageCount:     len(transcript.Messages),
				UserMessageCount: transcript.userMessageCount(),
				SessionName:      origin.spawn.Name,
				ParentSessionID:  parentID,
				RelationshipType: RelSubagent,
				File:             FileInfo{Path: child.path, Size: transcript.Size, Mtime: transcript.ModTime.UnixNano()},
			},
			Messages: transcript.Messages,
		})
	}
	return results, sourceErrs, nil
}

// withSpawnPrompt starts a subagent transcript with the prompt its spawning
// sub_agent_start delivered. kiro-cli records that prompt only in the
// spawning transcript, so without it a subagent's transcript starts mid-task.
// The copied turn is marked with the record type it came from.
func (t kiroCurrentTranscript) withSpawnPrompt(spawn kiroSubagentSpawn) kiroCurrentTranscript {
	prompt := strings.TrimSpace(spawn.Prompt)
	if prompt == "" {
		return t
	}
	messages := make([]ParsedMessage, 0, len(t.Messages)+1)
	messages = append(messages, ParsedMessage{
		Ordinal:       0,
		Role:          RoleUser,
		Content:       prompt,
		Timestamp:     spawn.Timestamp,
		ContentLength: len(prompt),
		SourceType:    "sub_agent_start",
	})
	for _, msg := range t.Messages {
		msg.Ordinal++
		messages = append(messages, msg)
	}
	t.Messages = messages
	t.FirstMessage = truncate(strings.ReplaceAll(prompt, "\n", " "), 300)
	if !spawn.Timestamp.IsZero() && (t.EarliestMessage.IsZero() || spawn.Timestamp.Before(t.EarliestMessage)) {
		t.EarliestMessage = spawn.Timestamp
	}
	return t
}

func kiroCurrentMessageTimestamp(value gjson.Result) time.Time {
	if ts := parseTimestamp(value.Str); !ts.IsZero() {
		return ts
	}
	if value.Type == gjson.Number {
		millis := value.Int()
		if millis != 0 {
			return time.UnixMilli(millis).UTC()
		}
	}
	return time.Time{}
}

// kiroExtractText extracts concatenated text from a Kiro message's
// content array.
func kiroExtractText(data gjson.Result) string {
	var parts []string
	data.Get("content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("kind").Str == "text" {
			if t := strings.TrimSpace(block.Get("data").Str); t != "" {
				parts = append(parts, t)
			}
		}
		return true
	})
	return strings.Join(parts, "\n\n")
}

// kiroExtractAssistant extracts text and tool calls from an
// AssistantMessage's content array.
func kiroExtractAssistant(
	data gjson.Result,
) (string, []ParsedToolCall) {
	var textParts []string
	var toolCalls []ParsedToolCall

	data.Get("content").ForEach(func(_, block gjson.Result) bool {
		switch block.Get("kind").Str {
		case "text":
			if t := strings.TrimSpace(block.Get("data").Str); t != "" {
				textParts = append(textParts, t)
			}
		case "toolUse":
			tu := block.Get("data")
			name := tu.Get("name").Str
			if name == "" {
				return true
			}
			inputJSON := tu.Get("input").Raw
			cat := NormalizeToolCategory(name)
			displayName := name
			// Normalize kiro-cli "write" tool to Edit/Write based on command
			if name == "write" {
				cmd := tu.Get("input.command").Str
				if cmd == "strReplace" {
					displayName = "Edit"
					cat = "Edit"
				} else {
					displayName = "Write"
				}
			}
			toolCalls = append(toolCalls, ParsedToolCall{
				ToolUseID: tu.Get("toolUseId").Str,
				ToolName:  displayName,
				Category:  cat,
				InputJSON: inputJSON,
			})
		}
		return true
	})

	return strings.Join(textParts, "\n\n"), toolCalls
}

// kiroExtractToolResults extracts tool results from a ToolResults
// message's content array.
func kiroExtractToolResults(
	data gjson.Result,
) []ParsedToolResult {
	var results []ParsedToolResult
	data.Get("content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("kind").Str != "toolResult" {
			return true
		}
		tr := block.Get("data")
		toolUseID := tr.Get("toolUseId").Str
		if toolUseID == "" {
			return true
		}
		contentRaw := tr.Get("content").Raw
		results = append(results, ParsedToolResult{
			ToolUseID:     toolUseID,
			ContentLength: len(contentRaw),
			ContentRaw:    contentRaw,
		})
		return true
	})
	return results
}

// kiroFormatToolCalls formats tool calls for display when there
// is no accompanying text.
func kiroFormatToolCalls(calls []ParsedToolCall) string {
	var parts []string
	for _, tc := range calls {
		parts = append(parts,
			formatToolHeader(tc.Category, tc.ToolName))
	}
	return strings.Join(parts, "\n")
}
