package plugin

// The gateway: OpenAI's chat completions as an agent harness asks for them,
// POST /api/otari/v1/chat/completions on the node. It is openrouter-qntx's
// route under this plugin's name.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/teranos/otari-qntx/otari"
	errors "github.com/teranos/sacred-error"
)

// GatewayPath is the route below /api/otari on the node.
const GatewayPath = "/v1/chat/completions"

// The node's word for who it admitted: the token that asked by its DID, and
// the user it reaches. A caller sending these is not believed: the node drops
// them and sets its own (QNTX plugin/grpc/client.go, askerHeaders).
const (
	headerAskerDID  = "X-Qntx-Asker-Did"
	headerAskerUser = "X-Qntx-Asker-User"
)

// maxAsked bounds one request body: the node's own bound on a plugin message.
const maxAsked = 100 << 20

// askerOf is who asked: the token, else the user. Empty is a node that admits
// without asking who.
func askerOf(h http.Header) string {
	if did := strings.TrimSpace(h.Get(headerAskerDID)); did != "" {
		return did
	}
	return strings.TrimSpace(h.Get(headerAskerUser))
}

// asUser is body with its `user` naming user, every other field as it came.
// On an otari service key that user is an end user: billed apart, capped at
// the key's end-user budget, under the key's ceiling.
func asUser(body []byte, user string) ([]byte, error) {
	var asked map[string]json.RawMessage
	if err := json.Unmarshal(body, &asked); err != nil {
		return nil, errors.Wrap(err, "the body is not a JSON object")
	}
	if asked == nil {
		return nil, errors.New("the body is not a JSON object")
	}
	named, err := json.Marshal(user)
	if err != nil {
		return nil, errors.Wrapf(err, "naming the user %q", user)
	}
	asked["user"] = named
	out, err := json.Marshal(asked)
	if err != nil {
		return nil, errors.Wrap(err, "writing the body with its user")
	}
	return out, nil
}

// refuse answers in the error shape OpenAI's clients read.
func refuse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}})
}

// gateway passes one chat completion on to otari with this plugin's key, never
// the caller's credential, and hands otari's answer back whole.
func (p *Plugin) gateway(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		refuse(w, http.StatusMethodNotAllowed, "chat completions are POSTed")
		return
	}
	if p.client == nil {
		refuse(w, http.StatusServiceUnavailable, "the otari plugin is not initialized")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAsked+1))
	if err != nil {
		refuse(w, http.StatusBadRequest, "the request body was not read: "+err.Error())
		return
	}
	if len(body) > maxAsked {
		refuse(w, http.StatusRequestEntityTooLarge, "the request body is larger than the node hands a plugin")
		return
	}
	// Whatever the caller put in `user` gives way to who the node admitted.
	if asker := askerOf(r.Header); asker != "" {
		if body, err = asUser(body, asker); err != nil {
			refuse(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	answered, err := p.client.PassOn(r.Context(), body)
	if err != nil {
		refuse(w, http.StatusBadGateway, "otari did not answer: "+err.Error())
		return
	}
	defer answered.Body.Close()
	// A streamed answer is read to its end: the node hands a plugin's answer
	// over in one piece.
	said, err := io.ReadAll(answered.Body)
	if err != nil {
		refuse(w, http.StatusBadGateway, "otari's answer was cut short: "+err.Error()+" [Otari-Request-ID "+answered.Header.Get(otari.HeaderRequestID)+"]")
		return
	}
	for name, values := range answered.Header {
		if name == "Content-Type" || name == "Retry-After" || strings.HasPrefix(name, "Otari-") {
			w.Header()[name] = values
		}
	}
	w.WriteHeader(answered.StatusCode)
	_, _ = w.Write(said)
}
