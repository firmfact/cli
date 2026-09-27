package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strconv"
	"strings"
)

// Streamable HTTP lets the server answer a POST as an event stream instead
// of one JSON body. firmfact does that for a chat: the model can take a
// minute or more, and while it works the server sends a ping every 15 s, so
// neither a proxy nor the CLI mistakes a slow answer for a dead connection.
// The JSON-RPC response comes as the last event.

// maxEvent bounds one event of a stream. The response event holds the whole
// tool result, and a long list comes to a few megabytes.
const maxEvent = 64 << 20

// isEventStream reports whether contentType is text/event-stream, with or
// without parameters such as a charset.
func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "text/event-stream"
}

// response is a JSON-RPC response, as a JSON body or one event of a stream.
type response struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// readEventStream reads an event stream (the WHATWG format: fields, a blank
// line ending each event) until the response to the request with id comes,
// and returns that. Everything else is skipped: comments, which proxies and
// servers send as keepalives, firmfact's own ping and started events,
// notifications, and responses to other requests. Lines end in LF or CRLF;
// a lone CR, which the format also allows, is not used by any server the
// CLI talks to.
func readEventStream(r io.Reader, id int) (*response, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxEvent)
	var (
		data  strings.Builder
		event string
	)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if msg := match(event, data.String(), id); msg != nil {
				return msg, nil
			}
			data.Reset()
			event = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			// Each data line adds its value and a newline; dispatch drops
			// the last newline.
			data.WriteString(value)
			data.WriteByte('\n')
			if data.Len() > maxEvent {
				return nil, errTooLarge
			}
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, errTooLarge
		}
		return nil, fmt.Errorf("the answer broke off: %w", err)
	}
	// An event not ended by a blank line is incomplete, and the format says
	// to drop it.
	return nil, errors.New("the server ended its answer before the result came; retry")
}

var errTooLarge = fmt.Errorf("the server's answer holds an event larger than %d MB", maxEvent>>20)

// match returns the response to request id if an event of the given type
// and data is one, and nil for any other event.
func match(event, data string, id int) *response {
	data = strings.TrimSuffix(data, "\n")
	if data == "" || (event != "" && event != "message") {
		return nil
	}
	// JSON-RPC allows a batch, an array of messages.
	var batch []json.RawMessage
	if json.Unmarshal([]byte(data), &batch) == nil {
		for _, raw := range batch {
			if msg := matchOne(raw, id); msg != nil {
				return msg
			}
		}
		return nil
	}
	return matchOne([]byte(data), id)
}

// matchOne returns raw as a response if it answers request id. A message
// with that id but neither result nor error is a request from the server,
// not the answer.
func matchOne(raw []byte, id int) *response {
	var msg response
	if json.Unmarshal(raw, &msg) != nil || !sameID(msg.ID, id) || (msg.Result == nil && msg.Error == nil) {
		return nil
	}
	return &msg
}

// sameID compares a JSON-RPC id with ours. The CLI sends numbers, which the
// server should echo as they are; the same number as a string is accepted
// too.
func sameID(raw json.RawMessage, id int) bool {
	got := strings.TrimSpace(string(raw))
	want := strconv.Itoa(id)
	return got == want || got == strconv.Quote(want)
}
