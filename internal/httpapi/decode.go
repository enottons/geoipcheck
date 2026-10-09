package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
)

// maxBodyBytes caps the request body. A full 250-country allowlist is
// about 1.5 KiB; 8 KiB leaves room without letting a client stream junk.
const maxBodyBytes = 8 << 10

const jsonMediaType = "application/json"

// decodeJSON strictly decodes the body into dst: JSON content type only, no
// unknown fields, exactly one value, at most maxBodyBytes. It returns the
// problem to send, or nil.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *problem {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != jsonMediaType {
		return &problem{
			Status: http.StatusUnsupportedMediaType,
			Code:   CodeUnsupportedMediaType,
			Detail: "Content-Type must be " + jsonMediaType,
		}
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeProblem(err)
	}
	// Anything after the first value means the client sent something other
	// than what it thinks it sent.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if p := tooLarge(err); p != nil {
			return p
		}
		return malformed("request body must contain a single JSON object")
	}
	return nil
}

// unknownFieldPrefix is how encoding/json reports DisallowUnknownFields
// violations. There is no typed error; the text has been stable since
// Go 1.10 and a test pins it.
const unknownFieldPrefix = "json: unknown field "

func decodeProblem(err error) *problem {
	if p := tooLarge(err); p != nil {
		return p
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		if typeErr.Field == "" {
			return malformed("request body must be a JSON object")
		}
		field := fieldPath(typeErr.Field)
		p := malformed(fmt.Sprintf("%s must be %s", field, jsonTypeName(typeErr.Type)))
		p.Field = field
		return p
	}
	if field, ok := strings.CutPrefix(err.Error(), unknownFieldPrefix); ok {
		field = strings.Trim(field, `"`)
		return &problem{
			Status: http.StatusBadRequest,
			Code:   CodeUnknownField,
			Field:  field,
			Detail: fmt.Sprintf("unknown field %q", field),
		}
	}
	if errors.Is(err, io.EOF) {
		return malformed("request body is empty")
	}
	return malformed("request body is not valid JSON")
}

func tooLarge(err error) *problem {
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		return nil
	}
	return &problem{
		Status: http.StatusRequestEntityTooLarge,
		Code:   CodeRequestTooLarge,
		Detail: fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes),
	}
}

func malformed(detail string) *problem {
	return &problem{Status: http.StatusBadRequest, Code: CodeMalformedJSON, Detail: detail}
}

// fieldPath rewrites encoding/json's "allowed_countries.0" into
// "allowed_countries[0]", the form countrycheck uses, so a client sees one
// field syntax whichever layer rejected the input.
func fieldPath(jsonPath string) string {
	var b strings.Builder
	for i, seg := range strings.Split(jsonPath, ".") {
		switch {
		case seg != "" && strings.Trim(seg, "0123456789") == "":
			b.WriteString("[" + seg + "]")
		case i > 0:
			b.WriteString("." + seg)
		default:
			b.WriteString(seg)
		}
	}
	return b.String()
}

// jsonTypeName names the JSON type a Go field expects, for error messages.
func jsonTypeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Slice, reflect.Array:
		return "an array of " + strings.TrimPrefix(jsonTypeName(t.Elem()), "a ") + "s"
	case reflect.Struct, reflect.Map:
		return "an object"
	default:
		return "a " + t.Kind().String()
	}
}
