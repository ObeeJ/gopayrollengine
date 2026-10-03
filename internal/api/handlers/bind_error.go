package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
)

// Validation errors are reported under each field's JSON name. By default the
// validator names the Go struct field ("Amount"), and gin's err.Error() prints
// "Key: 'Amount' Error:Field validation for 'Amount' failed on the 'required'
// tag": Go internals handed to a mobile client that cannot act on them.
func init() {
	if v, ok := binding.Validator.Engine().(*validator.Validate); ok {
		v.RegisterTagNameFunc(func(f reflect.StructField) string {
			name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
			if name == "-" || name == "" {
				return f.Name
			}
			return name
		})
	}
}

// respondBindError turns a ShouldBind* failure into a client-safe 400 (or 413
// when the body hit the size limit). Always use this instead of writing
// err.Error() for a binding failure.
func respondBindError(c *gin.Context, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body is too large"})
		return
	}

	var verrs validator.ValidationErrors
	if errors.As(err, &verrs) {
		fields := make(map[string]string, len(verrs))
		for _, fe := range verrs {
			fields[fe.Field()] = validationProblem(fe)
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": summarise(fields), "fields": fields})
		return
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		fields := map[string]string{field: "has the wrong type"}
		c.JSON(http.StatusBadRequest, gin.H{"error": summarise(fields), "fields": fields})
		return
	}

	// Strict decoders (json.Decoder.DisallowUnknownFields) report a stray field
	// as a plain error: `json: unknown field "is_active"`.
	if name, ok := unknownField(err); ok {
		fields := map[string]string{name: "is not a recognised field"}
		c.JSON(http.StatusBadRequest, gin.H{"error": summarise(fields), "fields": fields})
		return
	}

	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request body must be valid JSON"})
		return
	}

	// Anything else (custom UnmarshalJSON failures, e.g. a malformed amount).
	c.JSON(http.StatusBadRequest, gin.H{"error": "request body is invalid"})
}

func unknownField(err error) (string, bool) {
	const prefix = `json: unknown field "`
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) || !strings.HasSuffix(msg, `"`) {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(msg, prefix), `"`), true
}

func validationProblem(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "is required"
	case "email":
		return "must be a valid email address"
	case "min", "gte", "gt":
		return fmt.Sprintf("must be at least %s", fe.Param())
	case "max", "lte", "lt":
		return fmt.Sprintf("must be at most %s", fe.Param())
	case "oneof":
		return "must be one of: " + strings.ReplaceAll(fe.Param(), " ", ", ")
	default:
		return "is invalid"
	}
}

func summarise(fields map[string]string) string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+" "+fields[name])
	}
	return strings.Join(parts, "; ")
}
