package dispatch

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/store"
)

// A connector people sign in to acts as the person a task is for: whoever
// asked, or set up the schedule, card or folder watch. So an agent reaches
// at the service only what that person may. The control plane keeps each
// person's refresh token, sealed, and gets access tokens from it as tools
// need them. Neither ever reaches an agent.

// AccountCallbackPath is where a service sends people back after they sign
// in.
const AccountCallbackPath = "/auth/connector/callback"

// AccountCallbackURL is the address to register with the service.
func (d *Dispatcher) AccountCallbackURL() string { return d.opts.PublicURL + AccountCallbackPath }

// errNoAccount is a person who has not signed in to a connector.
var errNoAccount = errors.New("not signed in")

func accountName(connectionID, userID string) string {
	return "account:" + connectionID + ":" + userID
}

// perUser returns the adapter and connection of a connector people sign in
// to.
func (d *Dispatcher) perUser(c store.Connection) (connectors.PerUser, connectors.Connection, error) {
	p, ok := connectors.PerUserFor(connectors.Kind(c.Kind))
	if !ok || !c.PerUser {
		return nil, connectors.Connection{}, fmt.Errorf("%s does not let people sign in with their own accounts", c.Name)
	}
	conn, err := d.Conn(c)
	return p, conn, err
}

// SignInURL is where a person goes to sign in to a connector.
func (d *Dispatcher) SignInURL(c store.Connection, state, challenge string) (string, error) {
	p, conn, err := d.perUser(c)
	if err != nil {
		return "", err
	}
	return p.SignInURL(conn, d.AccountCallbackURL(), state, challenge)
}

// Connect redeems the code a person came back with, and keeps their
// sign-in, replacing any earlier one.
func (d *Dispatcher) Connect(ctx context.Context, c store.Connection, userID, code, verifier string) (store.Account, error) {
	p, conn, err := d.perUser(c)
	if err != nil {
		return store.Account{}, err
	}
	g, err := p.Exchange(ctx, conn, d.AccountCallbackURL(), code, verifier)
	if err != nil {
		return store.Account{}, err
	}
	a := store.Account{
		ConnectionID: c.ID, UserID: userID, Account: g.Account,
		Token: d.box.Seal(accountName(c.ID, userID), []byte(g.Refresh)),
	}
	if err := d.st.SaveAccount(ctx, a); err != nil {
		return store.Account{}, err
	}
	d.tokens.set(c.ID, userID, g)
	return a, nil
}

// Disconnect forgets a person's sign-in to a connector.
func (d *Dispatcher) Disconnect(ctx context.Context, connectionID, userID string) error {
	d.tokens.forget(connectionID, userID)
	return d.st.DeleteAccount(ctx, connectionID, userID)
}

// ForgetSignIns forgets everyone's sign-in to a connector, as when it moves
// to another server: their tokens must not be sent there. It returns how
// many there were. Other replicas read the account before each use, so
// deleting it stops them too.
func (d *Dispatcher) ForgetSignIns(ctx context.Context, connectionID string) (int64, error) {
	n, err := d.st.DeleteConnectionAccounts(ctx, connectionID)
	d.tokens.forgetConnection(connectionID)
	return n, err
}

// refreshTimeout bounds refreshing a person's token and storing the new one.
const refreshTimeout = 30 * time.Second

// asPerson gives a connection people sign in to the access token of the
// person a call is for. who names them in errors. Any other connection is
// returned as it is.
func (d *Dispatcher) asPerson(ctx context.Context, conn connectors.Connection, name, userID, who string) (connectors.Connection, error) {
	if !conn.PerUser {
		return conn, nil
	}
	if userID == "" {
		return conn, fmt.Errorf("%s acts as the person who asked, and nobody asked for this task", name)
	}
	who = cmp.Or(who, "the person who asked")
	p, ok := connectors.PerUserFor(conn.Kind)
	if !ok {
		return conn, fmt.Errorf("%s does not let people sign in with their own accounts", name)
	}
	a, err := d.st.Account(ctx, conn.ID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return conn, fmt.Errorf("%w: %s has not connected their account to %s yet. "+
			"They can, under Connectors in Keera Agents", errNoAccount, who, name)
	}
	if err != nil {
		return conn, err
	}
	again := fmt.Sprintf("%s needs to connect their account to %s again, under Connectors in Keera Agents", who, name)
	refresh, err := d.box.Open(accountName(conn.ID, userID), a.Token)
	if err != nil {
		return conn, errors.New(again)
	}
	access, err := d.tokens.get(ctx, conn.ID, userID, string(refresh), func(ctx context.Context) (connectors.Grant, error) {
		// A service that rotates refresh tokens has dropped the old one once
		// it answers, so the new one must be stored even if the call that
		// asked gives up meanwhile.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		// Read again under a lock: another replica may have rotated the
		// token since.
		var g connectors.Grant
		err := d.st.RotateAccountToken(ctx, conn.ID, userID, func(sealed []byte) ([]byte, error) {
			current, err := d.box.Open(accountName(conn.ID, userID), sealed)
			if err != nil {
				return nil, errors.New(again)
			}
			if g, err = p.Refresh(ctx, conn, string(current)); err != nil {
				return nil, fmt.Errorf("%w. If it persists, %s", err, again)
			}
			// A service that rotates refresh tokens accepts the old one no more.
			if g.Refresh != "" && g.Refresh != string(current) {
				return d.box.Seal(accountName(conn.ID, userID), []byte(g.Refresh)), nil
			}
			return nil, nil
		})
		if errors.Is(err, store.ErrNotFound) {
			err = errors.New(again)
		}
		return g, err
	})
	if err != nil {
		return conn, err
	}
	conn.Token, conn.Account = access, a.Account
	return conn, nil
}

// ConnFor turns a stored connection into what an adapter takes, as userID
// when people sign in to it, for something they do in the panel, such as
// binding the connector or browsing its folders.
func (d *Dispatcher) ConnFor(ctx context.Context, c store.Connection, userID string) (connectors.Connection, error) {
	conn, err := d.Conn(c)
	if err != nil {
		return conn, err
	}
	conn, err = d.asPerson(ctx, conn, c.Name, userID, "")
	if errors.Is(err, errNoAccount) {
		return conn, fmt.Errorf("connect your own account to %s first, under Connectors: it acts as each person", c.Name)
	}
	return conn, err
}

// CheckAccounts forgets the sign-ins of people who left their organisation
// or were disabled, so agents stop acting as them.
func (d *Dispatcher) CheckAccounts(ctx context.Context) {
	holders, err := d.st.AccountHolders(ctx)
	if err != nil {
		return
	}
	for _, h := range holders {
		u, err := d.gw.UserByID(ctx, h.OrgID, h.UserID)
		// Only the gateway's own answer counts: a stray 404 would sign
		// everyone out.
		if !gateway.IsNotFound(err) && (err != nil || !u.Disabled) {
			continue
		}
		d.tokens.forgetUser(h.UserID)
		n, err := d.st.DeleteUserAccounts(ctx, h.OrgID, h.UserID)
		if err != nil || n == 0 {
			continue
		}
		d.log.Info("forgot the sign-ins of someone who left", "org", h.OrgID, "user", h.UserID, "count", n)
		_ = d.st.Audit(ctx, h.OrgID, "keera-agents", "account.user_left", h.UserID,
			map[string]string{"count": strconv.FormatInt(n, 10)})
	}
}

// tokens keeps people's access tokens until shortly before they expire, so
// each tool call does not cost a refresh. Each person's is refreshed by one
// caller at a time: a service that rotates refresh tokens accepts each only
// once.
type tokens struct {
	mu sync.Mutex
	m  map[string]*heldToken
}

type heldToken struct {
	mu sync.Mutex
	// from is a hash of the refresh token the access token came from. A new
	// sign-in changes it, and so replaces the token.
	from [32]byte
	// replaced is a hash of the refresh token the last refresh rotated out.
	// A caller that read it before the refresh gets the new access token.
	replaced [32]byte
	access   string
	expiry   time.Time
}

// tokenEarly is how long before expiry a token is replaced, so it does not
// run out in the middle of a call.
const tokenEarly = 2 * time.Minute

func tokenKey(connectionID, userID string) string { return connectionID + "|" + userID }

func (t *tokens) held(connectionID, userID string) *heldToken {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*heldToken{}
	}
	key := tokenKey(connectionID, userID)
	h := t.m[key]
	if h == nil {
		h = &heldToken{}
		t.m[key] = h
	}
	return h
}

// get returns the access token got from refresh, or fetches a new one.
func (t *tokens) get(ctx context.Context, connectionID, userID, refresh string,
	fetch func(context.Context) (connectors.Grant, error),
) (string, error) {
	h := t.held(connectionID, userID)
	h.mu.Lock()
	defer h.mu.Unlock()
	from := sha256.Sum256([]byte(refresh))
	if (h.from == from || h.replaced == from) && h.access != "" && time.Now().Before(h.expiry.Add(-tokenEarly)) {
		return h.access, nil
	}
	g, err := fetch(ctx)
	if err != nil {
		return "", err
	}
	if g.Refresh != "" {
		if next := sha256.Sum256([]byte(g.Refresh)); next != from {
			h.replaced, from = from, next
		}
	}
	h.from, h.access, h.expiry = from, g.Access, g.Expiry
	return g.Access, nil
}

// set keeps the access token a sign-in gave.
func (t *tokens) set(connectionID, userID string, g connectors.Grant) {
	h := t.held(connectionID, userID)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.from, h.access, h.expiry = sha256.Sum256([]byte(g.Refresh)), g.Access, g.Expiry
	h.replaced = [32]byte{}
}

func (t *tokens) forget(connectionID, userID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, tokenKey(connectionID, userID))
}

func (t *tokens) forgetConnection(connectionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.m {
		if strings.HasPrefix(key, connectionID+"|") {
			delete(t.m, key)
		}
	}
}

func (t *tokens) forgetUser(userID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.m {
		if strings.HasSuffix(key, "|"+userID) {
			delete(t.m, key)
		}
	}
}
