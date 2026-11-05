package github

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/version"
)

// ClientID is the client ID of the OAuth app of the runplane project, with
// the device flow on. The setup signs the user in with it to list the
// accounts where the user can make the GitHub App. The device flow needs no
// client secret. The release build sets it with
// -ldflags "-X github.com/runplane/runplane/internal/github.ClientID=...".
var ClientID = ""

// signInScope lets the token read the organization memberships of the user.
const signInScope = "read:org"

var errNoClientID = errors.New("this runplane build has no OAuth client ID for a GitHub sign-in")

// signIn is a sign-in with the device flow. Its mutex makes the reads of one
// sign-in take turns, so runplane asks GitHub at most once in each interval.
type signIn struct {
	mu         sync.Mutex
	deviceCode string
	expires    time.Time
	interval   time.Duration
	next       time.Time // earliest time of the next token request
	token      string    // kept only until the accounts are read
	result     api.GitHubSignInResult
}

func (s *Service) CanSignIn() bool { return s.clientID != "" }

// StartSignIn asks GitHub for a device code. The user types its user code on
// GitHub, and SignInResult then gives the accounts of the user.
func (s *Service) StartSignIn(ctx context.Context) (api.GitHubSignIn, error) {
	if !s.CanSignIn() {
		return api.GitHubSignIn{}, errNoClientID
	}
	var out struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := s.oauth(ctx, "/login/device/code", url.Values{"client_id": {s.clientID}, "scope": {signInScope}}, &out); err != nil {
		return api.GitHubSignIn{}, err
	}
	now := time.Now()
	in := &signIn{
		deviceCode: out.DeviceCode,
		expires:    now.Add(time.Duration(out.ExpiresIn) * time.Second),
		interval:   time.Duration(out.Interval) * time.Second,
		result:     api.GitHubSignInResult{Status: "pending", Interval: out.Interval, Accounts: []api.GitHubAccount{}},
	}
	in.next = now.Add(in.interval)
	id := rand.Text()

	s.signMu.Lock()
	for k, old := range s.signIns {
		if now.After(old.expires) {
			delete(s.signIns, k)
		}
	}
	s.signIns[id] = in
	s.signMu.Unlock()
	return api.GitHubSignIn{ID: id, UserCode: out.UserCode, VerificationURI: out.VerificationURI, Interval: out.Interval}, nil
}

// SignInResult reads the state of the sign-in id. After the user approves
// runplane, it reads the accounts with the token, then drops the token. An
// unknown id, as after a restart, reads as expired.
func (s *Service) SignInResult(ctx context.Context, id string) (api.GitHubSignInResult, error) {
	s.signMu.Lock()
	in := s.signIns[id]
	s.signMu.Unlock()
	if in == nil {
		return api.GitHubSignInResult{Status: "expired", Accounts: []api.GitHubAccount{}}, nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if err := s.advance(ctx, in, time.Now()); err != nil {
		return api.GitHubSignInResult{}, err
	}
	return in.result, nil
}

// advance moves a pending sign-in on. in.mu must be held.
func (s *Service) advance(ctx context.Context, in *signIn, now time.Time) error {
	switch {
	case in.result.Status != "pending":
		return nil
	case in.token != "":
		// An earlier read got the token but not the accounts.
	case now.After(in.expires):
		in.result.Status = "expired"
		return nil
	case now.Before(in.next):
		return nil
	default:
		token, err := s.deviceToken(ctx, in, now)
		if err != nil || token == "" {
			return err
		}
		in.token = token
	}
	login, accounts, err := s.accounts(ctx, bearer(in.token))
	if err != nil {
		return err
	}
	in.token = ""
	in.result.Status, in.result.Login, in.result.Accounts = "done", login, accounts
	return nil
}

// deviceToken asks GitHub for the token of the sign-in. Until the user
// approves runplane, it returns "" and sets the next request time. When the
// sign-in ends without a token, it sets the status.
func (s *Service) deviceToken(ctx context.Context, in *signIn, now time.Time) (string, error) {
	var out struct {
		AccessToken string `json:"access_token"`
	}
	err := s.oauth(ctx, "/login/oauth/access_token", url.Values{
		"client_id":   {s.clientID},
		"device_code": {in.deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}, &out)
	var refused *oauthError
	if !errors.As(err, &refused) {
		return out.AccessToken, err
	}
	switch refused.Code {
	case "authorization_pending":
	case "slow_down":
		in.interval = cmp.Or(time.Duration(refused.Interval)*time.Second, in.interval+5*time.Second)
		in.result.Interval = int(in.interval.Seconds())
	case "expired_token":
		in.result.Status = "expired"
	case "access_denied":
		in.result.Status = "denied"
	default:
		return "", err
	}
	in.next = now.Add(in.interval)
	return "", nil
}

// accounts returns the login of the token's user and the accounts where the
// user can make the app: the organizations that the user owns, the personal
// account, then the other organizations.
func (s *Service) accounts(ctx context.Context, a auth) (string, []api.GitHubAccount, error) {
	var user account
	if err := s.api.json(ctx, call{method: http.MethodGet, path: "/user", auth: a}, &user); err != nil {
		return "", nil, fmt.Errorf("read the GitHub user: %w", err)
	}
	type membership struct {
		Role         string  `json:"role"`
		Organization account `json:"organization"`
	}
	orgs, err := list[membership](ctx, s.api, a, "/user/memberships/orgs?state=active&per_page=100", "")
	if err != nil {
		return "", nil, fmt.Errorf("read the organizations of %s: %w", user.Login, err)
	}
	slices.SortFunc(orgs, func(x, y membership) int {
		return strings.Compare(strings.ToLower(x.Organization.Login), strings.ToLower(y.Organization.Login))
	})
	var owned, others []api.GitHubAccount
	for _, m := range orgs {
		acc := api.GitHubAccount{Login: m.Organization.Login, Type: "organization", Owner: m.Role == "admin"}
		if acc.Owner {
			owned = append(owned, acc)
		} else {
			others = append(others, acc)
		}
	}
	personal := api.GitHubAccount{Login: user.Login, Type: "user", Owner: true}
	return user.Login, slices.Concat(owned, []api.GitHubAccount{personal}, others), nil
}

// bearer is the token of a signed-in user.
type bearer string

func (b bearer) header(context.Context) (string, error) { return "Bearer " + string(b), nil }
func (bearer) invalidate()                              {}

// oauthError is a refusal of a GitHub OAuth endpoint. GitHub sends the
// refusals of the device flow, such as authorization_pending, with status 200.
type oauthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
	Interval    int    `json:"interval"` // with slow_down
}

func (e *oauthError) Error() string {
	return "GitHub refused the sign-in: " + cmp.Or(e.Description, e.Code)
}

// oauth posts a form to an OAuth endpoint of GitHub and decodes the JSON
// answer into out. A refusal is an *oauthError.
func (s *Service) oauth(ctx context.Context, path string, form url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.web+path, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("make request POST %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "runplane/"+version.Version)
	resp, err := s.api.http.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("GitHub POST %s: %w", path, err)
	}
	var refused oauthError
	if json.Unmarshal(b, &refused) == nil && refused.Code != "" {
		return &refused
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub POST %s: %s", path, resp.Status)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decode GitHub POST %s: %w", path, err)
	}
	return nil
}
