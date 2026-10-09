package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

type orderedMap struct {
	keys   map[string]bool
	values map[string]any
}

func newOrderedMap() *orderedMap {
	return &orderedMap{keys: map[string]bool{}, values: map[string]any{}}
}
func (m *orderedMap) set(key string, value any) { m.keys[key] = true; m.values[key] = value }

type turnstileOp func([]any)

func solveTurnstileToken(encoded, key string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	plain := xorString(string(decoded), key)
	var tokens []any
	if err := json.Unmarshal([]byte(plain), &tokens); err != nil {
		return "", err
	}
	process := map[float64]any{}
	result := ""
	started := time.Now()
	process[1] = turnstileOp(func(args []any) {
		if len(args) >= 2 {
			process[number(args[0])] = xorString(valueString(process[number(args[0])]), valueString(process[number(args[1])]))
		}
	})
	process[2] = turnstileOp(func(args []any) {
		if len(args) >= 2 {
			process[number(args[0])] = args[1]
		}
	})
	process[3] = turnstileOp(func(args []any) {
		if len(args) >= 1 {
			result = base64.StdEncoding.EncodeToString([]byte(valueString(process[number(args[0])])))
		}
	})
	process[9] = tokens
	process[10] = "window"
	process[14] = turnstileOp(func(args []any) {
		if len(args) >= 2 {
			var value any
			if json.Unmarshal([]byte(valueString(process[number(args[1])])), &value) == nil {
				process[number(args[0])] = value
			}
		}
	})
	process[15] = turnstileOp(func(args []any) {
		if len(args) >= 2 {
			value, _ := json.Marshal(process[number(args[1])])
			process[number(args[0])] = string(value)
		}
	})
	process[16] = key
	process[17] = turnstileOp(func(args []any) { invokeTurnstile(process, args, started) })
	process[18] = turnstileOp(func(args []any) {
		if len(args) >= 1 {
			value, _ := base64.StdEncoding.DecodeString(valueString(process[number(args[0])]))
			process[number(args[0])] = string(value)
		}
	})
	process[19] = turnstileOp(func(args []any) {
		if len(args) >= 1 {
			process[number(args[0])] = base64.StdEncoding.EncodeToString([]byte(valueString(process[number(args[0])])))
		}
	})
	process[20] = turnstileOp(func(args []any) {
		if len(args) >= 3 && equalTurnstile(process[number(args[0])], process[number(args[1])]) {
			callTurnstile(process[number(args[2])], args[3:])
		}
	})
	process[21] = turnstileOp(func(args []any) {})
	process[23] = turnstileOp(func(args []any) {
		if len(args) >= 2 && process[number(args[0])] != nil {
			callTurnstile(process[number(args[1])], args[2:])
		}
	})
	process[24] = turnstileOp(func(args []any) {
		if len(args) >= 3 {
			left, right := process[number(args[1])], process[number(args[2])]
			if _, ok := left.(string); ok {
				if _, ok := right.(string); ok {
					process[number(args[0])] = valueString(left) + "." + valueString(right)
				}
			}
		}
	})
	for _, raw := range tokens {
		instruction, ok := raw.([]any)
		if !ok || len(instruction) == 0 {
			continue
		}
		callTurnstile(process[number(instruction[0])], instruction[1:])
	}
	return result, nil
}

func invokeTurnstile(process map[float64]any, args []any, started time.Time) {
	if len(args) < 2 {
		return
	}
	target, refIndex := number(args[0]), number(args[1])
	ref := process[refIndex]
	values := make([]any, 0, len(args)-2)
	for _, arg := range args[2:] {
		values = append(values, process[number(arg)])
	}
	switch ref {
	case "window.performance.now":
		process[target] = float64(time.Since(started).Microseconds()) / 1000
	case "window.Object.create":
		process[target] = newOrderedMap()
	case "window.Object.keys":
		if len(values) > 0 && values[0] == "window.localStorage" {
			process[target] = []any{"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4", "STATSIG_LOCAL_STORAGE_STABLE_ID", "client-correlated-secret", "oai/apps/capExpiresAt", "oai-did", "STATSIG_LOCAL_STORAGE_LOGGING_REQUEST", "UiState.isNavigationCollapsed.1"}
		}
	case "window.Math.random":
		process[target] = rand.New(rand.NewSource(time.Now().UnixNano())).Float64()
	default:
		callTurnstile(ref, values)
	}
}

func callTurnstile(value any, args []any) {
	if function, ok := value.(turnstileOp); ok {
		function(args)
	}
}
func number(value any) float64 {
	if result, ok := value.(float64); ok {
		return result
	}
	if result, ok := value.(int); ok {
		return float64(result)
	}
	return 0
}
func equalTurnstile(left, right any) bool { return fmt.Sprint(left) == fmt.Sprint(right) }
func xorString(text, key string) string {
	if key == "" {
		return text
	}
	source := []rune(key)
	output := make([]rune, 0, len([]rune(text)))
	for index, char := range []rune(text) {
		output = append(output, char^source[index%len(source)])
	}
	return string(output)
}
func valueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case string:
		switch typed {
		case "window.Math":
			return "[object Math]"
		case "window.Reflect":
			return "[object Reflect]"
		case "window.performance":
			return "[object Performance]"
		case "window.localStorage":
			return "[object Storage]"
		case "window.Object":
			return "function Object() { [native code] }"
		}
		return typed
	case float64:
		return fmt.Sprint(typed)
	case []string:
		return strings.Join(typed, ",")
	default:
		return fmt.Sprint(value)
	}
}
