package cli

import (
	"fmt"
	"io"
)

// Error is a failed command: what to say, the bullets under it, an optional
// hint, and the exit code the shell sees.
type Error struct {
	Msg     string
	Details []string
	Hint    string
	Code    int
}

// Failf is a validation or logic failure: the config is wrong, an engine did
// not answer, the broker refused the request. Exit 1.
func Failf(format string, a ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, a...), Code: ExitFail}
}

// Systemf is a system failure: bad flags, an unreadable file, berthd
// unreachable. The command could not do its job at all. Exit 2.
func Systemf(format string, a ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, a...), Code: ExitSystem}
}

// Detail adds one bullet under the error line.
func (e *Error) Detail(format string, a ...any) *Error {
	e.Details = append(e.Details, fmt.Sprintf(format, a...))
	return e
}

// WithHint adds the line that says what to try next.
func (e *Error) WithHint(format string, a ...any) *Error {
	e.Hint = fmt.Sprintf(format, a...)
	return e
}

func (e *Error) Error() string { return e.Msg }

// render writes the whole [ERROR] block: the message, one indented bullet per
// detail, then the hint.
func (e *Error) render(w io.Writer, s Style) {
	fmt.Fprintf(w, "%s %s\n", s.Err(markError), e.Msg)
	for _, d := range e.Details {
		fmt.Fprintf(w, "%s%s\n", errIndent, d)
	}
	if e.Hint != "" {
		fmt.Fprintf(w, "%s%s\n", errText, e.Hint)
	}
}
