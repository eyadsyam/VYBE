package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// DecodeJSON reads a JSON request body into dst, returning a *Problem on any
// failure so the handler can pass the error straight to WriteProblem.
//
// Three choices here are deliberate and each prevents a specific class of bug:
//
//   - **Unknown fields are rejected.** A client that sends `{"emial": ...}`
//     gets a 400 naming the field rather than a confusing validation error
//     about a missing email. It also means a field renamed on the server
//     breaks loudly at the first request instead of silently ignoring input.
//   - **Exactly one JSON value is required.** Without the trailing check,
//     `{"a":1}{"b":2}` decodes the first object and discards the rest —
//     request smuggling territory, and at best a client bug nobody notices.
//   - **The body is size-limited.** An unbounded decode lets one request
//     exhaust the process's memory, and the limit has to be applied to the
//     reader, not checked afterwards.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := requireJSONContentType(r); err != nil {
		return err
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return jsonDecodeProblem(err)
	}

	// Anything after the first value is a malformed request, not a courtesy.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrBadRequest.WithDetail("The body must contain exactly one JSON object.")
	}
	return nil
}

// MaxBodyBytes bounds a JSON request body at 1 MiB.
//
// Generous for every endpoint in the v1 surface — the largest is a room
// creation with a title — and small enough that a flood of maximal bodies
// cannot exhaust memory. Uploads, when they exist, will not go through this
// path.
const MaxBodyBytes = 1 << 20

func requireJSONContentType(r *http.Request) error {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		// An absent Content-Type on a body-carrying method is almost always a
		// misconfigured client, and guessing JSON would hide that.
		return ErrUnsupportedMedia.WithDetail("A Content-Type of application/json is required.")
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ErrUnsupportedMedia.WithDetail("The Content-Type header could not be parsed.")
	}
	// Accept application/json and any +json suffix, per RFC 6839.
	if mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
		return ErrUnsupportedMedia.WithDetail("Content-Type %q is not supported; use application/json.", mediaType)
	}
	return nil
}

// jsonDecodeProblem turns a json decode error into something a client can act on.
//
// encoding/json's messages are written for a Go developer reading a stack
// trace, not for a mobile client author reading a 400. The translation is
// worth the switch: "json: unknown field \"emial\"" becomes a message that
// names the field and says what to do.
func jsonDecodeProblem(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var maxBytesErr *http.MaxBytesError

	switch {
	case errors.As(err, &syntaxErr):
		return ErrBadRequest.WithCause(err).
			WithDetail("The body is not valid JSON (at byte %d).", syntaxErr.Offset)

	case errors.Is(err, io.ErrUnexpectedEOF):
		return ErrBadRequest.WithCause(err).
			WithDetail("The body ended in the middle of a JSON value.")

	case errors.As(err, &typeErr):
		field := typeErr.Field
		if field == "" {
			field = "(body)"
		}
		return ErrValidation.WithCause(err).
			WithDetail("Field %q must be a %s.", field, typeErr.Type.String()).
			WithFieldErrors(FieldError{
				Field:  field,
				Code:   "TYPE_MISMATCH",
				Detail: fmt.Sprintf("expected %s, got %s", typeErr.Type.String(), typeErr.Value),
			})

	case errors.Is(err, io.EOF):
		return ErrBadRequest.WithCause(err).WithDetail("The body is empty.")

	case errors.As(err, &maxBytesErr):
		return ErrPayloadTooLarge.WithCause(err).
			WithDetail("The body exceeds the %d byte limit.", MaxBodyBytes)

	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name := strings.TrimPrefix(err.Error(), "json: unknown field ")
		return ErrValidation.WithCause(err).
			WithDetail("Unknown field %s.", name).
			WithFieldErrors(FieldError{
				Field:  strings.Trim(name, `"`),
				Code:   "UNKNOWN_FIELD",
				Detail: "This field is not part of the request schema.",
			})

	default:
		return ErrBadRequest.WithCause(err).WithDetail("The body could not be decoded.")
	}
}
