// Package tokens estimates prompt/completion token counts with tiktoken BPE
// tables embedded in the binary, so estimation never needs network access.
// It is used only when an upstream provider does not report usage.
package tokens

import (
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktokenloader "github.com/pkoukk/tiktoken-go-loader"

	"github.com/XuLaiQ/all2api/internal/ports"
)

// Per-message overhead used by OpenAI's chat format accounting.
const (
	tokensPerMessage = 3
	tokensPerName    = 1
	tokensPerReply   = 3
)

var (
	loaderOnce sync.Once
	cacheMu    sync.Mutex
	cache      = map[string]*tiktoken.Tiktoken{}
)

// Estimate returns (promptTokens, completionTokens) for a chat exchange.
func Estimate(messages []ports.ChatMessage, completion, model string) (int, int) {
	encoding := encodingFor(model)
	if encoding == nil {
		return 0, 0
	}
	return countMessages(encoding, messages), len(encoding.Encode(completion, nil, nil))
}

// CountText returns the token count of a plain string for the given model.
func CountText(text, model string) int {
	encoding := encodingFor(model)
	if encoding == nil {
		return 0
	}
	return len(encoding.Encode(text, nil, nil))
}

func countMessages(encoding *tiktoken.Tiktoken, messages []ports.ChatMessage) int {
	total := 0
	for _, message := range messages {
		total += tokensPerMessage
		total += len(encoding.Encode(message.Role, nil, nil))
		total += len(encoding.Encode(message.Content, nil, nil))
		if message.ToolCallID != "" {
			total += len(encoding.Encode(message.ToolCallID, nil, nil)) + tokensPerName
		}
		for _, call := range message.ToolCalls {
			total += len(encoding.Encode(call.Name, nil, nil))
			total += len(encoding.Encode(call.Arguments, nil, nil))
		}
	}
	return total + tokensPerReply
}

// encodingFor resolves the tokenizer for an upstream model, falling back to
// o200k_base and then cl100k_base for models tiktoken does not know (which
// covers every non-OpenAI channel).
func encodingFor(model string) *tiktoken.Tiktoken {
	loaderOnce.Do(func() { tiktoken.SetBpeLoader(tiktokenloader.NewOfflineLoader()) })
	name := upstreamModel(model)
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if encoding, ok := cache[name]; ok {
		return encoding
	}
	encoding, err := tiktoken.EncodingForModel(name)
	if err != nil {
		encoding, err = tiktoken.GetEncoding(tiktoken.MODEL_O200K_BASE)
	}
	if err != nil {
		encoding, err = tiktoken.GetEncoding(tiktoken.MODEL_CL100K_BASE)
	}
	if err != nil {
		encoding = nil
	}
	cache[name] = encoding
	return encoding
}

// upstreamModel strips a gateway channel prefix such as "chatgpt/gpt-4o".
func upstreamModel(model string) string {
	model = strings.TrimSpace(model)
	if _, rest, found := strings.Cut(model, "/"); found {
		return rest
	}
	return model
}
