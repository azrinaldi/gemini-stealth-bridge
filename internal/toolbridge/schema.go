package toolbridge

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CheckSchema validates value against a JSON Schema subset matching tool-bridge.js.
func CheckSchema(schema any, value any, path string) string {
	var root map[string]any
	if sMap, ok := schema.(map[string]any); ok {
		root = sMap
	}
	return checkSchemaWithRoot(schema, root, value, path, 0)
}

func resolveSchemaRef(root map[string]any, ref string) (any, bool) {
	if root == nil || !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	var cur any = root
	for _, p := range parts {
		p = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok || m == nil {
			return nil, false
		}
		next, exists := m[p]
		if !exists {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func checkSchemaWithRoot(schema any, root map[string]any, value any, path string, depth int) string {
	if depth > 64 || schema == nil || schema == true {
		return ""
	}
	if b, ok := schema.(bool); ok && !b {
		return fmt.Sprintf("%s is not allowed", path)
	}
	sMap, ok := schema.(map[string]any)
	if !ok {
		return fmt.Sprintf("%s has an invalid schema definition", path)
	}
	if root == nil {
		root = sMap
	}

	if value == nil {
		if nullable, _ := sMap["nullable"].(bool); nullable {
			return ""
		}
	}

	if refStr, ok := sMap["$ref"].(string); ok && strings.TrimSpace(refStr) != "" {
		target, resolved := resolveSchemaRef(root, strings.TrimSpace(refStr))
		if !resolved {
			return fmt.Sprintf("%s failed unresolved schema $ref %q", path, refStr)
		}
		if errStr := checkSchemaWithRoot(target, root, value, path, depth+1); errStr != "" {
			return errStr
		}
	}

	if notSchema, exists := sMap["not"]; exists && notSchema != nil {
		if checkSchemaWithRoot(notSchema, root, value, path, depth+1) == "" {
			return fmt.Sprintf("%s matches forbidden not-schema", path)
		}
	}

	if tRaw, exists := sMap["type"]; exists && tRaw != nil {
		types := extractStringSlice(tRaw)
		if s, ok := tRaw.(string); ok {
			types = []string{s}
		}
		if len(types) > 0 {
			actual := jsonTypeOf(value)
			matched := false
			for _, t := range types {
				if t == actual || (t == "integer" && actual == "number" && isIntegerValue(value)) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Sprintf("%s must be %s (got %s)", path, strings.Join(types, "|"), actual)
			}
		}
	}

	if enumVals, ok := extractEnumSlice(sMap["enum"]); ok {
		matched := false
		for _, e := range enumVals {
			if jsonEqual(e, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Sprintf("%s is not one of the allowed values", path)
		}
	}

	if constVal, exists := sMap["const"]; exists {
		if !jsonEqual(constVal, value) {
			return fmt.Sprintf("%s must equal the declared constant", path)
		}
	}

	if strVal, ok := value.(string); ok {
		runeLen := utf8.RuneCountInString(strVal)
		if minLen, ok := intFromAny(sMap["minLength"]); ok && runeLen < minLen {
			return fmt.Sprintf("%s is shorter than %d", path, minLen)
		}
		if maxLen, ok := intFromAny(sMap["maxLength"]); ok && runeLen > maxLen {
			return fmt.Sprintf("%s is longer than %d", path, maxLen)
		}
		if pat, ok := sMap["pattern"].(string); ok && pat != "" {
			if re, err := regexp.Compile(pat); err == nil {
				if !re.MatchString(strVal) {
					return fmt.Sprintf("%s does not match the required pattern", path)
				}
			}
		}
	}

	if numVal, ok := floatFromAny(value); ok {
		if minVal, ok := floatFromAny(sMap["minimum"]); ok && numVal < minVal {
			return fmt.Sprintf("%s is below %v", path, minVal)
		}
		if maxVal, ok := floatFromAny(sMap["maximum"]); ok && numVal > maxVal {
			return fmt.Sprintf("%s is above %v", path, maxVal)
		}
		if exMin, ok := floatFromAny(sMap["exclusiveMinimum"]); ok && numVal <= exMin {
			return fmt.Sprintf("%s must be above %v", path, exMin)
		}
		if exMax, ok := floatFromAny(sMap["exclusiveMaximum"]); ok && numVal >= exMax {
			return fmt.Sprintf("%s must be below %v", path, exMax)
		}
		if mult, ok := floatFromAny(sMap["multipleOf"]); ok && mult > 0 {
			q := numVal / mult
			if math.Abs(q-math.Round(q)) > 1e-9 {
				return fmt.Sprintf("%s is not a multiple of %v", path, mult)
			}
		}
	}

	if arrVal, ok := value.([]any); ok {
		if minItems, ok := intFromAny(sMap["minItems"]); ok && len(arrVal) < minItems {
			return fmt.Sprintf("%s has fewer than %d items", path, minItems)
		}
		if maxItems, ok := intFromAny(sMap["maxItems"]); ok && len(arrVal) > maxItems {
			return fmt.Sprintf("%s has more than %d items", path, maxItems)
		}
		if uniq, _ := sMap["uniqueItems"].(bool); uniq {
			for i := range arrVal {
				for j := i + 1; j < len(arrVal); j++ {
					if jsonEqual(arrVal[i], arrVal[j]) {
						return fmt.Sprintf("%s has duplicate items", path)
					}
				}
			}
		}
		if prefixItems, ok := sMap["prefixItems"].([]any); ok {
			for i := range arrVal {
				if i < len(prefixItems) {
					if errStr := checkSchemaWithRoot(prefixItems[i], root, arrVal[i], fmt.Sprintf("%s[%d]", path, i), depth+1); errStr != "" {
						return errStr
					}
				} else if addItems, exists := sMap["items"]; exists {
					if errStr := checkSchemaWithRoot(addItems, root, arrVal[i], fmt.Sprintf("%s[%d]", path, i), depth+1); errStr != "" {
						return errStr
					}
				}
			}
		} else if itemsRaw, exists := sMap["items"]; exists && itemsRaw != nil {
			if itemsArr, ok := itemsRaw.([]any); ok {
				for i := range arrVal {
					var subSchema any
					if i < len(itemsArr) {
						subSchema = itemsArr[i]
					} else if addItems, hasAdd := sMap["additionalItems"]; hasAdd {
						subSchema = addItems
					}
					if errStr := checkSchemaWithRoot(subSchema, root, arrVal[i], fmt.Sprintf("%s[%d]", path, i), depth+1); errStr != "" {
						return errStr
					}
				}
			} else {
				for i := range arrVal {
					if errStr := checkSchemaWithRoot(itemsRaw, root, arrVal[i], fmt.Sprintf("%s[%d]", path, i), depth+1); errStr != "" {
						return errStr
					}
				}
			}
		}
	}

	if objVal, ok := value.(map[string]any); ok && objVal != nil {
		for _, reqKey := range extractStringSlice(sMap["required"]) {
			if _, exists := objVal[reqKey]; !exists {
				return fmt.Sprintf(`%s is missing required property %q`, path, reqKey)
			}
		}
		props, _ := sMap["properties"].(map[string]any)
		if props != nil {
			for k, sub := range props {
				if v, exists := objVal[k]; exists {
					if errStr := checkSchemaWithRoot(sub, root, v, path+"."+k, depth+1); errStr != "" {
						return errStr
					}
				}
			}
		}
		if addProps, ok := sMap["additionalProperties"].(bool); ok && !addProps {
			for k := range objVal {
				if props == nil {
					return fmt.Sprintf(`%s has unexpected property %q`, path, k)
				}
				if _, declared := props[k]; !declared {
					return fmt.Sprintf(`%s has unexpected property %q`, path, k)
				}
			}
		} else if addPropSchema, ok := sMap["additionalProperties"].(map[string]any); ok {
			for k, v := range objVal {
				if props != nil {
					if _, declared := props[k]; declared {
						continue
					}
				}
				if errStr := checkSchemaWithRoot(addPropSchema, root, v, path+"."+k, depth+1); errStr != "" {
					return errStr
				}
			}
		}
	}

	if allOf, ok := sMap["allOf"].([]any); ok {
		for _, sub := range allOf {
			if errStr := checkSchemaWithRoot(sub, root, value, path, depth+1); errStr != "" {
				return errStr
			}
		}
	}

	for _, kw := range []string{"anyOf", "oneOf"} {
		if branches, ok := sMap[kw].([]any); ok && len(branches) > 0 {
			matches := 0
			for _, sub := range branches {
				if checkSchemaWithRoot(sub, root, value, path, depth+1) == "" {
					matches++
				}
			}
			if matches == 0 {
				return fmt.Sprintf("%s matches no %s branch", path, kw)
			}
			if kw == "oneOf" && matches > 1 {
				return fmt.Sprintf("%s matches several oneOf branches", path)
			}
		}
	}

	return ""
}

func extractStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func jsonTypeOf(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case bool:
		return "boolean"
	case float64, int, int64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func isIntegerValue(v any) bool {
	switch n := v.(type) {
	case int, int64:
		return true
	case float64:
		return !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n
	default:
		return false
	}
}

func intFromAny(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if math.Trunc(n) == n {
			return int(n), true
		}
	}
	return 0, false
}

func floatFromAny(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func jsonEqual(a, b any) bool {
	ba, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ba) == string(bb)
}

func extractEnumSlice(raw any) ([]any, bool) {
	switch v := raw.(type) {
	case []any:
		return v, true
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out, true
	case []int:
		out := make([]any, len(v))
		for i, n := range v {
			out[i] = n
		}
		return out, true
	}
	return nil, false
}

var nonFunctionalSchemaKeys = map[string]bool{
	"$schema":  true,
	"title":    true,
	"examples": true,
	"$id":      true,
	"$comment": true,
}

// TruncateUTF8 truncates s on a UTF-8 rune boundary to at most maxRunes runes,
// appending "..." if truncated.
func TruncateUTF8(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	limit := maxRunes - 3
	if limit < 1 {
		limit = 1
	}
	count := 0
	for idx := range s {
		if count >= limit {
			return s[:idx] + "..."
		}
		count++
	}
	return s
}

// CompactSchemaMap recursively copies a JSON Schema map while stripping
// non-functional metadata keys ($schema, title, boolean additionalProperties, examples, $id, $comment)
// and truncating "description" strings to maxDescChars runes. Object-valued
// additionalProperties schemas are compacted and preserved. User-controlled
// names under properties, patternProperties, $defs, and definitions are kept
// verbatim; literal keyword values are never interpreted as schema maps.
func CompactSchemaMap(raw map[string]any, maxDescChars int) map[string]any {
	if raw == nil {
		return nil
	}
	if maxDescChars <= 0 {
		maxDescChars = 400
	}
	return compactSchemaNode(raw, maxDescChars)
}

func compactSchemaNode(raw map[string]any, maxDescChars int) map[string]any {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		if nonFunctionalSchemaKeys[k] {
			continue
		}
		if k == "additionalProperties" {
			if addMap, ok := v.(map[string]any); ok && addMap != nil {
				out[k] = copySchemaValue(addMap, maxDescChars)
			}
			continue
		}
		switch k {
		case "description":
			if str, ok := v.(string); ok {
				v = TruncateUTF8(str, maxDescChars)
			}
		case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies":
			if entries, ok := v.(map[string]any); ok && entries != nil {
				names := make(map[string]any, len(entries))
				for name, schema := range entries {
					names[name] = copySchemaValue(schema, maxDescChars)
				}
				out[k] = names
				continue
			}
		case "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "items", "prefixItems",
			"additionalItems", "contains", "propertyNames", "unevaluatedItems", "unevaluatedProperties",
			"contentSchema":
			out[k] = copySchemaValue(v, maxDescChars)
			continue
		}
		// Other keyword values, including const, default, and enum, contain data.
		out[k] = copySchemaValue(v, 0)
	}
	return out
}

// A zero limit copies JSON data without interpreting it as a schema.
func copySchemaValue(v any, maxDescChars int) any {
	switch val := v.(type) {
	case map[string]any:
		if val == nil {
			return val
		}
		if maxDescChars > 0 {
			return compactSchemaNode(val, maxDescChars)
		}
		cp := make(map[string]any, len(val))
		for k, child := range val {
			cp[k] = copySchemaValue(child, 0)
		}
		return cp
	case []any:
		if val == nil {
			return val
		}
		cp := make([]any, len(val))
		for i, item := range val {
			cp[i] = copySchemaValue(item, maxDescChars)
		}
		return cp
	default:
		return v
	}
}

// CoerceArgumentsToSchema attempts lossless, schema-guided type coercion on
// properties in args that would otherwise fail CheckSchema (for example string
// "50" -> 50 for integer, "true" -> true for boolean, JSON-encoded array/object
// strings, or scalar string -> []string when an array is required). Values that
// already satisfy their schema are never modified.
func CoerceArgumentsToSchema(schema map[string]any, args map[string]any) {
	if schema == nil || args == nil {
		return
	}
	coerceMapWithRoot(schema, schema, args, 0)
}

func coerceMapWithRoot(schema map[string]any, root map[string]any, obj map[string]any, depth int) {
	if depth > 16 || schema == nil || obj == nil {
		return
	}
	if ref, ok := schema["$ref"].(string); ok && ref != "" {
		if resolved, found := resolveSchemaRef(root, ref); found {
			if rMap, ok2 := resolved.(map[string]any); ok2 {
				schema = rMap
			}
		}
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return
	}
	for k, val := range obj {
		propSchema, exists := props[k]
		if !exists || propSchema == nil {
			continue
		}
		if checkSchemaWithRoot(propSchema, root, val, k, 0) == "" {
			continue
		}
		if coerced, ok := coerceValueWithRoot(propSchema, root, val, k, depth+1); ok {
			if checkSchemaWithRoot(propSchema, root, coerced, k, 0) == "" {
				obj[k] = coerced
			}
		}
	}
}

func coerceValueWithRoot(schema any, root map[string]any, val any, path string, depth int) (any, bool) {
	if depth > 16 || schema == nil {
		return val, false
	}
	sMap, ok := schema.(map[string]any)
	if !ok || sMap == nil {
		return val, false
	}
	if ref, ok := sMap["$ref"].(string); ok && ref != "" {
		if resolved, found := resolveSchemaRef(root, ref); found {
			return coerceValueWithRoot(resolved, root, val, path, depth+1)
		}
	}
	for _, kw := range []string{"anyOf", "oneOf"} {
		if branches, ok := sMap[kw].([]any); ok && len(branches) > 0 {
			for _, br := range branches {
				if cVal, ok2 := coerceValueWithRoot(br, root, val, path, depth+1); ok2 {
					if checkSchemaWithRoot(sMap, root, cVal, path, 0) == "" {
						return cVal, true
					}
				}
			}
		}
	}

	allowedTypes := extractStringSlice(sMap["type"])
	if tStr, ok := sMap["type"].(string); ok && tStr != "" {
		allowedTypes = []string{tStr}
	}
	if len(allowedTypes) == 0 {
		if _, hasProps := sMap["properties"]; hasProps {
			allowedTypes = []string{"object"}
		} else if _, hasItems := sMap["items"]; hasItems {
			allowedTypes = []string{"array"}
		}
	}

	switch v := val.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		for _, t := range allowedTypes {
			switch t {
			case "integer":
				if iVal, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
					cand := float64(iVal)
					if checkSchemaWithRoot(sMap, root, cand, path, 0) == "" {
						return cand, true
					}
				}
				if fVal, err := strconv.ParseFloat(trimmed, 64); err == nil && !math.IsNaN(fVal) && !math.IsInf(fVal, 0) && math.Trunc(fVal) == fVal {
					if checkSchemaWithRoot(sMap, root, fVal, path, 0) == "" {
						return fVal, true
					}
				}
			case "number":
				if fVal, err := strconv.ParseFloat(trimmed, 64); err == nil && !math.IsNaN(fVal) && !math.IsInf(fVal, 0) {
					if checkSchemaWithRoot(sMap, root, fVal, path, 0) == "" {
						return fVal, true
					}
				}
			case "boolean":
				switch strings.ToLower(trimmed) {
				case "true":
					if checkSchemaWithRoot(sMap, root, true, path, 0) == "" {
						return true, true
					}
				case "false":
					if checkSchemaWithRoot(sMap, root, false, path, 0) == "" {
						return false, true
					}
				}
			case "array":
				if strings.HasPrefix(trimmed, "[") {
					if arr, ok := unmarshalCandidateArray(trimmed); ok {
						if itemSchema := sMap["items"]; itemSchema != nil {
							for i, elem := range arr {
								if checkSchemaWithRoot(itemSchema, root, elem, path, 0) != "" {
									if cElem, ok2 := coerceValueWithRoot(itemSchema, root, elem, path, depth+1); ok2 {
										arr[i] = cElem
									}
								}
							}
						}
						if checkSchemaWithRoot(sMap, root, arr, path, 0) == "" {
							return arr, true
						}
					} else if strings.HasSuffix(trimmed, "]") {
						var emptyArr []any
						if err := json.Unmarshal([]byte(trimmed), &emptyArr); err == nil && emptyArr != nil && len(emptyArr) == 0 {
							if checkSchemaWithRoot(sMap, root, emptyArr, path, 0) == "" {
								return emptyArr, true
							}
							return val, false
						}
					}
				}
				if v != "" {
					var arr []any
					if strings.Contains(v, "\n") {
						for _, line := range strings.Split(v, "\n") {
							if s := strings.TrimSpace(line); s != "" {
								arr = append(arr, s)
							}
						}
					}
					if len(arr) == 0 {
						arr = []any{v}
					}
					if checkSchemaWithRoot(sMap, root, arr, path, 0) == "" {
						return arr, true
					}
				}
			case "object":
				if strings.HasPrefix(trimmed, "{") {
					if parsedObj, ok := unmarshalCandidateObject(trimmed); ok {
						coerceMapWithRoot(sMap, root, parsedObj, depth+1)
						if checkSchemaWithRoot(sMap, root, parsedObj, path, 0) == "" {
							return parsedObj, true
						}
					}
				}
			}
		}
	case float64:
		for _, t := range allowedTypes {
			if t == "string" {
				s := strconv.FormatFloat(v, 'f', -1, 64)
				if checkSchemaWithRoot(sMap, root, s, path, 0) == "" {
					return s, true
				}
			}
		}
	case bool:
		for _, t := range allowedTypes {
			if t == "string" {
				s := strconv.FormatBool(v)
				if checkSchemaWithRoot(sMap, root, s, path, 0) == "" {
					return s, true
				}
			}
		}
	case map[string]any:
		cp := make(map[string]any, len(v))
		for mk, mv := range v {
			cp[mk] = mv
		}
		coerceMapWithRoot(sMap, root, cp, depth+1)
		if checkSchemaWithRoot(sMap, root, cp, path, 0) == "" {
			return cp, true
		}
	case []any:
		if itemSchema := sMap["items"]; itemSchema != nil {
			cp := make([]any, len(v))
			changed := false
			for i, elem := range v {
				cp[i] = elem
				if checkSchemaWithRoot(itemSchema, root, elem, path, 0) != "" {
					if cElem, ok2 := coerceValueWithRoot(itemSchema, root, elem, path, depth+1); ok2 {
						cp[i] = cElem
						changed = true
					}
				}
			}
			if changed && checkSchemaWithRoot(sMap, root, cp, path, 0) == "" {
				return cp, true
			}
		}
	}
	return val, false
}
