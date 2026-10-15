// Package httpx 提供 HTTP 契约层的公共小工具：JSON 读写、请求体访问、路由参数与登录身份。
//
// 与旧实现 Express 的行为对齐要点：
//   - 请求体在鉴权之前解析（app.use(express.json) 先于路由），解析/超限错误统一 500 服务器内部错误；
//   - handlers 通过 Body 拿到已解析的对象（application/json 或 x-www-form-urlencoded）。
package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

type ctxKey int

const (
	userKey ctxKey = iota
	bodyKey
	paramsKey
)

// User 是本服务注入的登录身份。
type User struct {
	Role string
	Name string
	// Via 标记身份来源：builtin（本地会话）/ gateway（X-Auth-User）。
	Via string
}

// WithUser 把身份写入请求上下文。
func WithUser(r *http.Request, u User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userKey, u))
}

// User 读取当前请求身份；未登录返回零值与 false。
func UserFrom(r *http.Request) (User, bool) {
	u, ok := r.Context().Value(userKey).(User)
	return u, ok
}

// WithBody 把已解析的请求体对象写入上下文。
func WithBody(r *http.Request, body map[string]any) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), bodyKey, body))
}

// Body 返回已解析的请求体对象（永远是对象，空请求体为 {}）。
func Body(r *http.Request) map[string]any {
	if b, ok := r.Context().Value(bodyKey).(map[string]any); ok && b != nil {
		return b
	}
	return map[string]any{}
}

// WithParams 把路径参数写入上下文。
func WithParams(r *http.Request, params map[string]string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), paramsKey, params))
}

// Param 读取路径参数。
func Param(r *http.Request, name string) string {
	if p, ok := r.Context().Value(paramsKey).(map[string]string); ok {
		return p[name]
	}
	return ""
}

// WriteJSON 输出 JSON（字符集与旧实现一致）。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// Error 输出 {"error": msg}。
func Error(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]any{"error": msg})
}

// BodyString 读取请求体里的字符串字段（缺失/非字符串回空串）。
func BodyString(body map[string]any, key string) string {
	switch v := body[key].(type) {
	case string:
		return v
	case []any:
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				return s
			}
		}
	case []string:
		if len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// BodyRaw 读取请求体里的原始字段（供需要区分「缺省」与「空」的场景）。
func BodyRaw(body map[string]any, key string) (any, bool) {
	v, ok := body[key]
	return v, ok
}

// BodyBool 读取布尔字段（严格 === true 语义由调用方判断原始值，这里只做类型断言）。
func BodyBool(body map[string]any, key string) (bool, bool) {
	v, ok := body[key].(bool)
	return v, ok
}

// ParseBody 解析请求体。contentType 为 application/json 时按 JSON 解析，
// x-www-form-urlencoded 时按表单解析；其它类型返回空对象。strict 与旧实现一致：
// JSON 顶层必须是对象，否则视为解析失败。返回 error 表示调用方应回 500。
func ParseBody(contentType string, maxBytes int64, body []byte) (map[string]any, error) {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case strings.HasPrefix(ct, "application/json"):
		trimmed := strings.TrimSpace(string(body))
		if trimmed == "" {
			return map[string]any{}, nil
		}
		var parsed any
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, err
		}
		obj, ok := parsed.(map[string]any)
		if !ok {
			return nil, errStrictJSON
		}
		return obj, nil
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for k, vs := range values {
			if len(vs) == 1 {
				out[k] = vs[0]
			} else {
				arr := make([]any, len(vs))
				for i, v := range vs {
					arr[i] = v
				}
				out[k] = arr
			}
		}
		return out, nil
	default:
		return map[string]any{}, nil
	}
}

type strictJSONError struct{}

func (strictJSONError) Error() string { return "JSON 顶层必须是对象" }

var errStrictJSON = strictJSONError{}
