package gateway

import (
	"bytes"
	"encoding/json"
)

const (
	maxUsageJSONBytes = 1 << 20
	maxSSELineBytes   = 64 << 10
	maxSSEEventBytes  = 64 << 10
)

type observedUsage struct {
	found        bool
	inputKnown   bool
	outputKnown  bool
	totalKnown   bool
	inputTokens  uint64
	outputTokens uint64
	totalTokens  uint64
}

type usageFields struct {
	PromptTokens     *uint64 `json:"prompt_tokens"`
	CompletionTokens *uint64 `json:"completion_tokens"`
	InputTokens      *uint64 `json:"input_tokens"`
	OutputTokens     *uint64 `json:"output_tokens"`
	TotalTokens      *uint64 `json:"total_tokens"`
}

func usageFromResponseJSON(data []byte) observedUsage {
	var response struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &response) != nil || len(response.Usage) == 0 || bytes.Equal(response.Usage, []byte("null")) {
		return observedUsage{}
	}
	return usageFromValue(response.Usage)
}

func usageFromValue(data []byte) observedUsage {
	var fields usageFields
	if json.Unmarshal(data, &fields) != nil {
		return observedUsage{}
	}
	var usage observedUsage
	if fields.PromptTokens != nil {
		usage.inputTokens = *fields.PromptTokens
		usage.inputKnown = true
	} else if fields.InputTokens != nil {
		usage.inputTokens = *fields.InputTokens
		usage.inputKnown = true
	}
	if fields.CompletionTokens != nil {
		usage.outputTokens = *fields.CompletionTokens
		usage.outputKnown = true
	} else if fields.OutputTokens != nil {
		usage.outputTokens = *fields.OutputTokens
		usage.outputKnown = true
	}
	if fields.TotalTokens != nil {
		usage.totalTokens = *fields.TotalTokens
		usage.totalKnown = true
	} else if usage.inputKnown && usage.outputKnown {
		usage.totalTokens = usage.inputTokens + usage.outputTokens
		if usage.totalTokens < usage.inputTokens {
			usage.totalTokens = ^uint64(0)
		}
		usage.totalKnown = true
	}
	usage.found = usage.inputKnown || usage.outputKnown || usage.totalKnown
	return usage
}

type responseUsageObserver struct {
	sse          bool
	jsonData     []byte
	jsonTooLarge bool
	line         []byte
	lineTooLong  bool
	eventData    []byte
	eventTooLong bool
	usage        observedUsage
}

func newResponseUsageObserver(sse bool) *responseUsageObserver {
	return &responseUsageObserver{sse: sse, line: make([]byte, 0, 256), eventData: make([]byte, 0, 512)}
}

func (observer *responseUsageObserver) observe(data []byte) {
	if !observer.sse {
		if !observer.jsonTooLarge {
			if len(observer.jsonData)+len(data) > maxUsageJSONBytes {
				observer.jsonData = nil
				observer.jsonTooLarge = true
			} else {
				observer.jsonData = append(observer.jsonData, data...)
			}
		}
		return
	}
	for _, value := range data {
		if value == '\n' {
			if observer.lineTooLong {
				observer.eventTooLong = true
			} else {
				observer.processSSELine()
			}
			observer.line = observer.line[:0]
			observer.lineTooLong = false
			continue
		}
		if observer.lineTooLong {
			continue
		}
		if len(observer.line) >= maxSSELineBytes {
			observer.lineTooLong = true
			observer.eventTooLong = true
			observer.line = observer.line[:0]
			continue
		}
		observer.line = append(observer.line, value)
	}
}

func (observer *responseUsageObserver) finish() observedUsage {
	if !observer.sse {
		if observer.jsonTooLarge {
			return observedUsage{}
		}
		return usageFromResponseJSON(observer.jsonData)
	}
	if observer.lineTooLong {
		observer.eventTooLong = true
	} else if len(observer.line) > 0 {
		observer.processSSELine()
	}
	observer.processSSEEvent()
	return observer.usage
}

func (observer *responseUsageObserver) processSSELine() {
	line := bytes.TrimSuffix(observer.line, []byte{'\r'})
	if len(line) == 0 {
		observer.processSSEEvent()
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := line[len("data:"):]
	if len(data) > 0 && data[0] == ' ' {
		data = data[1:]
	}
	separator := 0
	if len(observer.eventData) > 0 {
		separator = 1
	}
	if len(observer.eventData)+separator+len(data) > maxSSEEventBytes {
		observer.eventTooLong = true
		observer.eventData = observer.eventData[:0]
		return
	}
	if separator > 0 {
		observer.eventData = append(observer.eventData, '\n')
	}
	observer.eventData = append(observer.eventData, data...)
}

func (observer *responseUsageObserver) processSSEEvent() {
	if !observer.eventTooLong && len(observer.eventData) > 0 {
		if usage := usageFromResponseJSON(observer.eventData); usage.found {
			observer.usage = usage
		}
	}
	observer.eventData = observer.eventData[:0]
	observer.eventTooLong = false
}
