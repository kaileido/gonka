package filters

import (
	"bytes"

	json "github.com/goccy/go-json"
)

type promptBody struct {
	Messages []promptMessage `json:"messages"`
	Tools    promptField     `json:"tools"`
}

type promptMessage struct {
	Content      promptField  `json:"content"`
	ToolCalls    []promptCall `json:"tool_calls"`
	FunctionCall *promptCall  `json:"function_call"`
}

type promptCall struct {
	Function  *promptCall `json:"function"`
	Name      promptField `json:"name"`
	Arguments promptField `json:"arguments"`
}

type promptField string

func (f *promptField) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*f = promptField(text)
		return nil
	}
	if !bytes.Equal(data, jsonNull) {
		*f = promptField(data)
	}
	return nil
}

// PromptText returns the text of a normalized body a host's chat template renders: message contents, tool calls and tool definitions, without the template itself. See README.md, "The prompt text".
func PromptText(body []byte) ([]string, error) {
	var decoded promptBody
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, err
	}
	parts := make([]string, 0, len(decoded.Messages)+1)
	add := func(field promptField) {
		if field != "" {
			parts = append(parts, string(field))
		}
	}
	addCall := func(call *promptCall) {
		if call.Function != nil {
			call = call.Function
		}
		add(call.Name)
		add(call.Arguments)
	}
	for index := range decoded.Messages {
		message := &decoded.Messages[index]
		add(message.Content)
		for call := range message.ToolCalls {
			addCall(&message.ToolCalls[call])
		}
		if message.FunctionCall != nil {
			addCall(message.FunctionCall)
		}
	}
	add(decoded.Tools)
	return parts, nil
}
