package testjsonl

import "maps"

// Kiro CLI current-layout transcripts wrap every record in an envelope with
// an optional record id, a timestamp, and a typed payload. These builders
// write that envelope with ordered structs so the output bytes are stable,
// which matters to tests that pin a source fingerprint hash. An empty
// timestamp or record id leaves the field out.

type kiroRecord struct {
	ID        string `json:"id,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Payload   any    `json:"payload"`
}

type kiroTextPayload struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type kiroToolCallPayload struct {
	Type       string `json:"type"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Args       any    `json:"args"`
}

type kiroToolResultPayload struct {
	Type       string `json:"type"`
	ToolCallID string `json:"toolCallId"`
	Content    any    `json:"content"`
}

type kiroSubAgentStartPayload struct {
	Type              string `json:"type"`
	ParentExecutionID string `json:"parentExecutionId"`
	SubSessionID      string `json:"subSessionId"`
	SubAgentName      string `json:"subAgentName"`
	Prompt            string `json:"prompt"`
	Explanation       string `json:"explanation"`
}

// KiroSubAgentExplanation is the explanation every KiroSubAgentStartJSON
// record carries; the parser reports it as the spawn's Task description.
const KiroSubAgentExplanation = "delegate the subtask"

// KiroUserJSON returns a Kiro CLI user record.
func KiroUserJSON(timestamp, content string) string {
	return mustMarshal(kiroRecord{
		Timestamp: timestamp,
		Payload:   kiroTextPayload{Type: "user", Content: content},
	})
}

// KiroAssistantJSON returns a Kiro CLI assistant record.
func KiroAssistantJSON(timestamp, content string) string {
	return mustMarshal(kiroRecord{
		Timestamp: timestamp,
		Payload:   kiroTextPayload{Type: "assistant", Content: content},
	})
}

// KiroToolCallJSON returns a Kiro CLI tool_call record for tool name with
// call id and the given args value.
func KiroToolCallJSON(timestamp, id, name string, args any) string {
	return mustMarshal(kiroRecord{
		Timestamp: timestamp,
		Payload: kiroToolCallPayload{
			Type: "tool_call", ToolCallID: id, ToolName: name, Args: args,
		},
	})
}

// KiroToolResultJSON returns a Kiro CLI tool_result record answering call id
// with the given content value.
func KiroToolResultJSON(timestamp, id string, content any) string {
	return mustMarshal(kiroRecord{
		Timestamp: timestamp,
		Payload: kiroToolResultPayload{
			Type: "tool_result", ToolCallID: id, Content: content,
		},
	})
}

// KiroSubAgentStartJSON returns the sub_agent_start record kiro-cli writes
// into the spawning transcript when it spawns subagent session subID. The
// record explanation is KiroSubAgentExplanation.
func KiroSubAgentStartJSON(
	recordID, timestamp, subID, name, prompt string,
) string {
	return mustMarshal(kiroRecord{
		ID:        recordID,
		Timestamp: timestamp,
		Payload: kiroSubAgentStartPayload{
			Type:              "sub_agent_start",
			ParentExecutionID: "exec-1",
			SubSessionID:      subID,
			SubAgentName:      name,
			Prompt:            prompt,
			Explanation:       KiroSubAgentExplanation,
		},
	})
}

// KiroSubAgentCompleteJSON returns the sub_agent_complete record kiro-cli
// writes into the spawning transcript when subagent session subID finishes.
// fields adds payload members such as status, response, and errorMessage; a
// nil value is written as JSON null and an absent key leaves the member out.
func KiroSubAgentCompleteJSON(
	recordID, timestamp, subID string, fields map[string]any,
) string {
	payload := map[string]any{
		"type":              "sub_agent_complete",
		"parentExecutionId": "exec-1",
		"subSessionId":      subID,
	}
	maps.Copy(payload, fields)
	return mustMarshal(kiroRecord{
		ID:        recordID,
		Timestamp: timestamp,
		Payload:   payload,
	})
}
