package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// tokenPlaceholder stands where the token goes on every visit that did not
// itself mint one. It is deliberately shouty rather than subtle: a person who
// copies a block without pressing the button should find out from their client
// immediately, not from a deploy that fails an hour later (AC-12).
const tokenPlaceholder = "YOUR_TOKEN_HERE"

// serverName is what the MCP server is called in every block. One name across
// four clients, so a person moving between machines types the same thing.
const serverName = "deployer"

// nameOrdinalLimit bounds how many dated names one mint will try before handing
// the refusal back. Somebody minting fifty tokens from one tab on one day has a
// different problem than a name collision.
const nameOrdinalLimit = 50

// connectClient is one coding agent's configuration block: how it is labelled,
// where the text belongs on that person's machine, and how the text itself is
// composed.
//
// The block is built by a Go function taking the endpoint and the credential,
// rather than by a template or a stored string, so the endpoint cannot be typed
// into the text by hand: it arrives as an argument or it does not appear at all
// (AC-9, AC-11).
type connectClient struct {
	// Key is the tab's id and the value the mint form posts back.
	Key string
	// Label is what the tab reads, and the head of the default name a token
	// minted from this tab is given (AC-16).
	Label string
	// Where names the file or the shell the block belongs in.
	Where string
	// Block composes the text. endpoint is the deploy path's address, token is
	// either a freshly minted value or the placeholder.
	Block func(endpoint, token string) string
}

// connectClients is the whole tab set, in the order the page renders them. The
// first is the one selected on arrival (AC-7).
//
// Three of these four formats belong to somebody else's release schedule and
// will go stale silently: nothing here can tell that a client changed the shape
// of its configuration, only that the endpoint inside it followed the
// platform's (AC-9). That is the standing cost of naming clients rather than
// showing one generic block, and spec 0023 records it rather than leaving it to
// be discovered.
var connectClients = []connectClient{
	{
		Key:   "claude-code",
		Label: "Claude Code",
		Where: "Run this once, in any terminal.",
		// A command line rather than a configuration file: Claude Code owns where
		// its own configuration lives, so handing over the command is one step
		// instead of three (AC-8).
		Block: func(endpoint, token string) string {
			return fmt.Sprintf("claude mcp add --transport http %s %s --header %q",
				serverName, endpoint, "Authorization: Bearer "+token)
		},
	},
	{
		Key:   "codex",
		Label: "Codex",
		Where: "Add this to ~/.codex/config.toml",
		Block: func(endpoint, token string) string {
			return fmt.Sprintf("[mcp_servers.%s]\nurl = %q\nhttp_headers = { \"Authorization\" = %q }",
				serverName, endpoint, "Bearer "+token)
		},
	},
	{
		Key:   "gemini-cli",
		Label: "Gemini CLI",
		Where: "Add this to ~/.gemini/settings.json",
		Block: func(endpoint, token string) string {
			return fmt.Sprintf(`{
  "mcpServers": {
    %q: {
      "httpUrl": %q,
      "headers": { "Authorization": %q }
    }
  }
}`, serverName, endpoint, "Bearer "+token)
		},
	},
	{
		Key:   "mcp-json",
		Label: "Other (MCP JSON)",
		Where: "For any client that takes a plain MCP server entry.",
		Block: func(endpoint, token string) string {
			return fmt.Sprintf(`{
  "mcpServers": {
    %q: {
      "type": "http",
      "url": %q,
      "headers": { "Authorization": %q }
    }
  }
}`, serverName, endpoint, "Bearer "+token)
		},
	},
}

// genericClient is the tab an unknown or absent form value resolves to. A stale
// or tampered field costs a tab selection and never a mint (AC-17).
var genericClient = connectClients[len(connectClients)-1]

// clientFor matches a posted tab value against the set. Anything it does not
// know is the generic tab rather than a refusal.
func clientFor(key string) connectClient {
	for _, c := range connectClients {
		if c.Key == key {
			return c
		}
	}
	return genericClient
}

// connectBlock is one rendered tab.
type connectBlock struct {
	Key      string
	Label    string
	Where    string
	Text     string
	Selected bool
}

// connectPageData is the four blocks and, on the one response that mints, the
// state that response alone carries. The raw token is not a field here: it is
// inside the block text and nowhere else, which is what stops a later render
// having anything to put back (AC-12, AC-15).
type connectPageData struct {
	Blocks []connectBlock
	// Minted is whether this response is the one holding a real token, which is
	// what the page reads to say so out loud.
	Minted bool
	// MintedName is the token's name in the list, so a person setting up a
	// second machine can tell the two apart afterwards.
	MintedName string
	Message    string
}

// connectPage hands a signed in person their finished configuration, and stamps
// the account on the way through so the sign in redirect fires exactly once.
//
// The stamp is skipped when the account already carries one, which is a read of
// the row this request already resolved rather than a query of its own. That is
// not what makes the stamp land once: the store's statement is conditional, so
// two first visits racing each other still leave exactly one (AC-4, AC-4a).
func (s *Server) connectPage(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if !account.Connected {
		if err := s.svc.MarkConnected(r.Context(), account.ID); err != nil {
			s.internalError(w, r, err, "stamping an account connected failed")
			return
		}
	}
	s.renderConnect(w, r, account, sess, http.StatusOK, "", tokenPlaceholder, connectPageData{})
}

// connectMint mints a token and re renders the page with the raw value inside
// every block. It follows POST /tokens exactly: no redirect, because a redirect
// would have to carry the value in a URL or hold it somewhere between two
// requests (AC-15).
func (s *Server) connectMint(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if !s.checkCSRF(w, r, account, sess) {
		return
	}
	client := clientFor(r.PostFormValue("client"))

	minted, err := s.mintForClient(r.Context(), account, client)
	if err != nil {
		code, refusal := identity.CodeOf(err)
		if !refusal {
			s.internalError(w, r, err, "minting a token from the connect page failed")
			return
		}
		var e *identity.Error
		errors.As(err, &e)
		s.renderConnect(w, r, account, sess, statusFor(code), client.Key, tokenPlaceholder,
			connectPageData{Message: e.Message})
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		ClientAddress: s.clientAddress(r),
		AccountID:     account.ID, Action: auth.ActionTokenMint, Allowed: true,
		TargetType: "api_token", TargetID: minted.Token.ID,
	})
	s.renderConnect(w, r, account, sess, http.StatusOK, client.Key, minted.Raw,
		connectPageData{Minted: true, MintedName: minted.Token.Name})
}

// mintForClient mints an ordinary token named for the tab it came from and
// today's date, so two machines are distinguishable in the token list afterwards
// (AC-16).
//
// A name an account already holds live is refused by identity, which is exactly
// what setting up a second machine on the same day would hit, so the dated name
// gains an ordinal and the mint is tried again rather than handed back as a
// refusal (AC-16a). Only that one code is retried: every other refusal is the
// caller's answer.
func (s *Server) mintForClient(ctx context.Context, account auth.Account,
	client connectClient,
) (identity.Minted, error) {
	base := client.Label + " " + s.svc.Now().UTC().Format("2006-01-02")
	var err error
	for n := 1; n <= nameOrdinalLimit; n++ {
		name := base
		if n > 1 {
			name = base + " (" + strconv.Itoa(n) + ")"
		}
		var minted identity.Minted
		minted, err = s.svc.MintToken(ctx, toIdentityAccount(account), name, 0)
		if err == nil {
			return minted, nil
		}
		if code, refusal := identity.CodeOf(err); !refusal || code != identity.CodeTokenNameTaken {
			return identity.Minted{}, err
		}
	}
	return identity.Minted{}, err
}

// renderConnect composes all four blocks and renders them. selected names the
// tab shown on arrival, and empty means the first one, which is Claude Code
// (AC-7).
//
// token is the whole difference between a visit and a mint. Every caller but the
// successful mint passes the placeholder, so a later request has no past value
// to render because it holds none (AC-12).
func (s *Server) renderConnect(w http.ResponseWriter, r *http.Request, account auth.Account,
	sess auth.Session, status int, selected, token string, data connectPageData,
) {
	// Derived from the configured deploy host, never written into a block, so a
	// hostname change moves one value and all four blocks follow (AC-9).
	endpoint := s.opts.MCPURL + "/mcp"
	if selected == "" {
		selected = connectClients[0].Key
	}
	data.Blocks = make([]connectBlock, 0, len(connectClients))
	for _, c := range connectClients {
		data.Blocks = append(data.Blocks, connectBlock{
			Key: c.Key, Label: c.Label, Where: c.Where,
			Text:     c.Block(endpoint, token),
			Selected: c.Key == selected,
		})
	}
	s.render(w, r, account, sess, status, "connect", "connect", data)
}
