package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// asked sends the gateway one request as the node hands it to the plugin: the
// path below /api/otari, and the headers the node set.
func asked(p *Plugin, method, body string, headers map[string]string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	_ = p.RegisterHTTP(mux)
	req := httptest.NewRequest(method, GatewayPath, strings.NewReader(body))
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

const askedBody = `{"model":"fast","stream":false,"user":"whoever-the-caller-says","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}],"max_tokens":9007199254740993}`

func TestGateway_WhoTheNodeAdmittedIsOtarisUser(t *testing.T) {
	f := &fakeOtari{}
	p := newTestPlugin(t, f)

	w := asked(p, http.MethodPost, askedBody, map[string]string{
		headerAskerDID:  "did:key:z6MkToken",
		headerAskerUser: "user-1",
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, f.got, 1)
	assert.Equal(t, "did:key:z6MkToken", f.got[0]["user"], "the token that asked, not what the caller wrote")
	assert.Equal(t, "fast", f.got[0]["model"])
	assert.Len(t, f.got[0]["tools"], 1, "what was asked goes on as it came")
	assert.Equal(t, "req-123", w.Header().Get("Otari-Request-ID"))
	assert.Equal(t, "secondary", w.Header().Get("Otari-Provider"))
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	var answer struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &answer))
	require.Len(t, answer.Choices, 1)
	assert.Equal(t, "Hello", answer.Choices[0].Message.Content)
}

func TestGateway_ASessionIsItsUser(t *testing.T) {
	f := &fakeOtari{}
	p := newTestPlugin(t, f)

	w := asked(p, http.MethodPost, askedBody, map[string]string{headerAskerUser: "user-1"})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "user-1", f.got[0]["user"])
}

func TestGateway_ANodeThatNamesNobodyPassesTheBodyAsItCame(t *testing.T) {
	f := &fakeOtari{}
	p := newTestPlugin(t, f)

	w := asked(p, http.MethodPost, askedBody, nil)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "whoever-the-caller-says", f.got[0]["user"])
}

func TestGateway_AFieldKeepsItsText(t *testing.T) {
	out, err := asUser([]byte(askedBody), "did:key:z6MkToken")
	require.NoError(t, err)
	assert.Contains(t, string(out), `"max_tokens":9007199254740993`, "a number is not re-rendered through a float")
}

func TestGateway_OtarisRefusalComesBackWhole(t *testing.T) {
	f := &fakeOtari{status: http.StatusForbidden, body: `{"detail":"User 'did:key:z6MkToken' has exceeded budget limit","code":"budget_exceeded"}`}
	p := newTestPlugin(t, f)

	w := asked(p, http.MethodPost, askedBody, map[string]string{headerAskerDID: "did:key:z6MkToken"})

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "pricing_required", w.Header().Get("Otari-Error-Code"), "the header otari set, as it set it")
	assert.Equal(t, "req-123", w.Header().Get("Otari-Request-ID"))
	assert.JSONEq(t, f.body, w.Body.String())
}

func TestGateway_RefusesWhatIsNotAChatCompletion(t *testing.T) {
	f := &fakeOtari{}
	p := newTestPlugin(t, f)

	assert.Equal(t, http.StatusMethodNotAllowed, asked(p, http.MethodGet, "", nil).Code)
	notObject := asked(p, http.MethodPost, `["not","an","object"]`, map[string]string{headerAskerUser: "user-1"})
	assert.Equal(t, http.StatusBadRequest, notObject.Code)
	assert.Contains(t, notObject.Body.String(), "not a JSON object")
	assert.Empty(t, f.got, "nothing reached otari")

	assert.Equal(t, http.StatusServiceUnavailable, asked(New(), http.MethodPost, askedBody, nil).Code)
}
