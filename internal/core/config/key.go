package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Type is the value type of a registered key. It selects the parser that
// coerces a raw value into its Go type, and the accepted-forms hint the
// operator sees when that fails.
type Type int

const (
	// TypeString is an uninterpreted string.
	TypeString Type = iota
	// TypeInt is a base-10 integer.
	TypeInt
	// TypeBool is a boolean in any spelling strconv.ParseBool accepts.
	TypeBool
	// TypeDuration is a Go duration string ("10s", "1m30s").
	TypeDuration
)

// String returns the type's name as it appears in operator-facing messages.
func (t Type) String() string {
	switch t {
	case TypeString:
		return "string"
	case TypeInt:
		return "integer"
	case TypeBool:
		return "boolean"
	case TypeDuration:
		return "duration"
	default:
		return "unknown"
	}
}

// Key is one registered configuration key. Registration is what makes the
// loader generic: core owns the precedence machinery and knows nothing about
// which keys exist, so an adapter declares its own without editing core.
//
// The flag and environment variable names are *derived* from Name rather than
// stored here — see FlagName and EnvName. That is deliberate: the dotted-key
// convention is then enforced by construction, so the second adapter cannot
// invent a second one. It also means a key cannot be registered with a flag
// name that disagrees with its config-file name.
type Key struct {
	// Name is the dotted, camelCase-segmented config key: "dhcp.commandTimeout".
	//
	// Two conventions bind it, because the derivations below read it literally.
	// Segments are ASCII — the split walks byte offsets, which is exact for
	// identifiers and undefined for anything else. And acronyms are spelled as
	// words: "disableHttps", never "disableHTTPS", which would derive
	// -disable-h-t-t-p-s. The existing core keys already follow both.
	Name string
	// Type selects the parser used for every source.
	Type Type
	// Default is the built-in value. A nil Default means the key has none and
	// resolves to its zero value unless a source sets it; requiring it is then
	// a validation concern, not a loading one.
	Default any
	// Usage is the flag's help text.
	Usage string
}

// Spec is an ordered set of registered keys. A binary composes one from
// CoreKeys plus its adapter's own.
type Spec []Key

// reservedFlag is the one flag the loader owns rather than deriving, so no key
// may claim it.
const reservedFlag = "config"

// index maps each key's name to its declaration, rejecting any registration
// that would resolve ambiguously.
//
// All three checks exist because the alternative is silence. Two keys of one
// name would resolve by declaration order. Two *differently* named keys can
// still derive one flag and one variable — dhcp.commandTimeout and
// dhcp.command.timeout both yield -dhcp-command-timeout and
// WEAVE_ADAPTER_DHCP_COMMAND_TIMEOUT — leaving the loser unsettable with
// nothing reported, which name-only checking misses precisely because the names
// do differ. And a key deriving the flag name "config" would make
// flag.StringVar panic with "flag redefined" rather than say what is wrong.
//
// Collision is tested once, on the shared word split, rather than once per
// derived name: FlagName and EnvName are both pure functions of words(), so two
// keys collide in one exactly when they collide in the other. Checking both
// separately would leave the second branch unreachable.
//
// A spec is composed from core plus an adapter, so these collisions are between
// packages that cannot see each other's key lists. Catching them at load is
// what makes that composition safe.
func (s Spec) index() (map[string]Key, error) {
	out := make(map[string]Key, len(s))
	byWords := make(map[string]string, len(s))

	for _, k := range s {
		if _, dup := out[k.Name]; dup {
			return nil, fmt.Errorf("duplicate config key %q", k.Name)
		}

		split := strings.Join(words(k.Name), "\x00")

		if FlagName(k.Name) == reservedFlag {
			return nil, fmt.Errorf("config key %q derives the reserved flag -%s", k.Name, reservedFlag)
		}

		if other, dup := byWords[split]; dup {
			return nil, fmt.Errorf("config keys %q and %q both derive the flag -%s and %s",
				other, k.Name, FlagName(k.Name), EnvName(k.Name))
		}

		out[k.Name] = k
		byWords[split] = k.Name
	}

	return out, nil
}

// FlagName renders a key as its CLI flag: dhcp.commandTimeout becomes
// dhcp-command-timeout.
func FlagName(key string) string {
	return strings.Join(words(key), "-")
}

// EnvName renders a key as its environment variable:
// dhcp.commandTimeout becomes WEAVE_ADAPTER_DHCP_COMMAND_TIMEOUT.
func EnvName(key string) string {
	return envPrefix + strings.ToUpper(strings.Join(words(key), "_"))
}

// words splits a dotted camelCase key into its lowercase words, so that both
// name derivations agree on where the boundaries are.
func words(key string) []string {
	var out []string

	for segment := range strings.SplitSeq(key, ".") {
		start := 0

		for i, r := range segment {
			if i > 0 && r >= 'A' && r <= 'Z' {
				out = append(out, strings.ToLower(segment[start:i]))
				start = i
			}
		}

		out = append(out, strings.ToLower(segment[start:]))
	}

	return out
}

// coerce converts a raw value into the key's Go type. Every source funnels
// through here: defaults, flags and TOML deliver typed values, the environment
// delivers strings, and one path handles both.
//
// This is where the loader stops trusting koanf's typed getters. k.Bool and
// k.Int discard a parse failure and answer the zero value, so
// DISABLE_AUTH=yes would silently become "authentication stays on" — failing
// safe, but leaving an operator who asked for something and was told nothing
// with no way to discover it was ignored.
func coerce(k Key, raw any) (any, error) {
	if raw == nil {
		return zeroOf(k.Type), nil
	}

	switch k.Type {
	case TypeString:
		return coerceString(k, raw)
	case TypeInt:
		return coerceInt(k, raw)
	case TypeBool:
		return coerceBool(k, raw)
	case TypeDuration:
		return coerceDuration(k, raw)
	default:
		return nil, fmt.Errorf("config key %q has an unknown type %d", k.Name, k.Type)
	}
}

// coerceString requires an actual string rather than stringifying whatever
// arrived. Accepting anything would turn `logSeverity = 5` into "5" and a TOML
// array into "[a b]", both of which then fail validation somewhere further
// away, describing a value the operator never wrote.
func coerceString(k Key, raw any) (any, error) {
	if v, ok := raw.(string); ok {
		return v, nil
	}

	return nil, typeError(k, raw, "a quoted string")
}

// coerceInt reads an integer from a native numeric value or a string. TOML
// yields int64, so the native cases are not hypothetical.
func coerceInt(k Key, raw any) (any, error) {
	switch v := raw.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case string:
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return nil, typeError(k, v, "a whole number like 8444")
		}

		return parsed, nil
	default:
		return nil, typeError(k, raw, "a whole number like 8444")
	}
}

// coerceBool reads a boolean from a native bool or any spelling ParseBool
// accepts.
func coerceBool(k Key, raw any) (any, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return nil, typeError(k, v, "true/false, 1/0, t/f, T/F, TRUE/FALSE")
		}

		return parsed, nil
	default:
		return nil, typeError(k, raw, "true/false, 1/0, t/f, T/F, TRUE/FALSE")
	}
}

// coerceDuration reads a duration from a native time.Duration (flags and
// defaults) or a Go duration string (TOML and the environment).
func coerceDuration(k Key, raw any) (any, error) {
	switch v := raw.(type) {
	case time.Duration:
		return v, nil
	case string:
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return nil, typeError(k, v, `a duration like "10s", "500ms" or "1m30s"`)
		}

		return parsed, nil
	default:
		return nil, typeError(k, raw, `a duration like "10s", "500ms" or "1m30s"`)
	}
}

// typeError renders a coercion failure naming the key, what was found, and the
// accepted forms. It names the environment variable too, because that is the
// source a value most often arrives from wrongly — a TOML file gets its types
// from the parser, and a flag from the flag package.
func typeError(k Key, got any, accepts string) error {
	return fmt.Errorf("%s (%s) is not a valid %s: expected %s, got %q",
		k.Name, EnvName(k.Name), k.Type, accepts, fmt.Sprint(got))
}

// zeroOf returns the zero value for a type, used for a key with no default
// that no source set.
func zeroOf(t Type) any {
	switch t {
	case TypeString:
		return ""
	case TypeInt:
		return 0
	case TypeBool:
		return false
	case TypeDuration:
		return time.Duration(0)
	default:
		return nil
	}
}
