package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/ui"
)

type signupOptions struct {
	Email string
	Name  string
	// Password comes from the prompt or --password-stdin, never from an
	// argument (see ReadStdinLine).
	Password      string
	PasswordStdin bool
	NoPassword    bool
	Org           string
	Type          string
	Currency      string
	Language      string
	AcceptTerms   bool
	Code          string
	CodeStdin     bool
	NoWait        bool
	Timing        bool
	WaitTimeout   time.Duration
}

// signupCodeEnv carries the emailed code for scripts. The environment of a
// process is only readable by its own user, unlike its arguments.
const signupCodeEnv = "FIRMFACT_SIGNUP_CODE"

type signupChoices struct {
	OrganizationTypes []struct {
		Value string `json:"value"`
		Label string `json:"label"`
	} `json:"organization_types"`
	Currencies []struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	} `json:"currencies"`
	DefaultCurrency string `json:"default_currency"`
	Languages       []struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	} `json:"languages"`
	Terms []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"terms"`
}

func newSignupCommand(app *App) *cobra.Command {
	o := &signupOptions{}
	cmd := &cobra.Command{
		Use:   "signup",
		Short: "Create a firmfact account",
		Long: `Create a firmfact account with your work email.

Signup creates two workspaces: your own (production) and a Demo workspace
filled with sample data. We email you a 6-digit code; once you enter it the
CLI is signed in and waits until the Demo workspace is ready.

A script signs up in two runs. The first passes the details as flags; it has
the code emailed and exits 0. The second passes --email and the code, and
confirms it without sending another email; the other details may be left out.
Keep the password and the code out of the arguments, where other users of the
machine could read them: pipe the password to --password-stdin, and give the
code in FIRMFACT_SIGNUP_CODE or with --code-stdin. Anything missing is asked
for when the CLI runs in a terminal, the password without echo.`,
		Example: fmt.Sprintf(`  %[1]s signup
  %[1]s signup --email jan@yourfirm.example --name "Jan de Vries" --org "Bank BV" \
    --type financial_services --currency EUR --language en --accept-terms --password-stdin < password.txt
  FIRMFACT_SIGNUP_CODE=123456 %[1]s signup --email jan@yourfirm.example`, app.Name),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("password") {
				return usageErrorf("--password is no longer accepted, as other users of this machine can read a command's arguments; pipe the password to --password-stdin, or leave it out to be asked for it")
			}
			return runSignup(cmd, app, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Email, "email", "", "your work email address")
	f.StringVar(&o.Name, "name", "", "your full name")
	f.BoolVar(&o.PasswordStdin, "password-stdin", false, "read the password for the web sign-in from standard input (one line)")
	f.BoolVar(&o.NoPassword, "no-password", false, "sign up without a password (set one later with \"Forgot password\" on the web)")
	// --password put the password in the arguments. It stays, hidden, only
	// so that an old script is told what to use instead; its value is never
	// read or shown.
	f.String("password", "", "")
	_ = f.MarkHidden("password")
	f.StringVar(&o.Org, "org", "", "your organisation's legal name")
	f.StringVar(&o.Type, "type", "", "organisation type, which picks the Demo data (see the prompt for choices)")
	f.StringVar(&o.Currency, "currency", "", "base currency, e.g. EUR or USD")
	f.StringVar(&o.Language, "language", "", "interface language, e.g. en or nl")
	f.BoolVar(&o.AcceptTerms, "accept-terms", false, "accept the terms of service, privacy policy and DPA")
	f.StringVar(&o.Code, "code", "", "the 6-digit code from the email, to finish a signup started earlier (asked for when omitted; scripts should prefer FIRMFACT_SIGNUP_CODE or --code-stdin, which other users cannot see)")
	f.BoolVar(&o.CodeStdin, "code-stdin", false, "read the 6-digit code from standard input (one line)")
	f.BoolVar(&o.NoWait, "no-wait", false, "do not wait for the Demo workspace to be ready (check later with `workspaces status`)")
	f.BoolVar(&o.Timing, "timing", false, "print how long each step took")
	f.DurationVar(&o.WaitTimeout, "wait-timeout", 20*time.Minute, "how long to wait for the Demo workspace")
	cmd.MarkFlagsMutuallyExclusive("password-stdin", "no-password")
	cmd.MarkFlagsMutuallyExclusive("code", "code-stdin")
	return cmd
}

// readSignupSecrets fills in the password and the code from standard input
// and the environment, before anything is sent, so a script that pipes the
// wrong thing fails at once.
func readSignupSecrets(ctx context.Context, app *App, o *signupOptions) error {
	if o.PasswordStdin && o.CodeStdin {
		return usageErrorf("--password-stdin and --code-stdin cannot both read standard input; give the code in %s instead", signupCodeEnv)
	}
	if o.PasswordStdin {
		pw, err := app.ReadStdinLine(ctx, "password-stdin", "password")
		if err != nil {
			return err
		}
		o.Password = pw
	}
	if o.CodeStdin {
		code, err := app.ReadStdinLine(ctx, "code-stdin", "code")
		if err != nil {
			return err
		}
		o.Code = code
	}
	if o.Code == "" {
		o.Code = os.Getenv(signupCodeEnv)
	}
	return nil
}

func runSignup(cmd *cobra.Command, app *App, o *signupOptions) error {
	ctx := cmd.Context()
	if err := readSignupSecrets(ctx, app, o); err != nil {
		return err
	}
	p, err := app.profileToChange(cmd)
	if err != nil {
		return err
	}
	c, err := app.Client()
	if err != nil {
		return err
	}
	interactive := app.Interactive()
	timer := newStepTimer(app, o.Timing)
	app.Banner()

	if strings.TrimSpace(o.Code) != "" {
		// A code finishes a signup that an earlier run started, as a
		// script's second run does. Sending the details again would only
		// email the same code again, against the per-address limit.
		if o.Email == "" && interactive {
			if o.Email, err = app.Prompt(ctx, "Work email", ""); err != nil {
				return err
			}
		}
		if o.Email == "" {
			return usageErrorf("missing --email: a code is confirmed for the address it was sent to")
		}
	} else if codeSent, err := startSignup(ctx, app, c, o, interactive, timer); err != nil || !codeSent {
		return err
	}
	token, account, err := confirmSignup(ctx, app, c, o, interactive, timer)
	if err != nil || token == nil {
		return err
	}

	if app.keepsSignIn(p, c.Host) {
		rememberHost(p, c.Host)
		p.Workspace = account.ID
		app.SaveConfig()
	} else {
		app.noteSignInNotKept(p, c.Host)
	}
	fmt.Fprintf(app.Out, "%s You are signed in, with %q as your default workspace.\n",
		app.Mode().Rainbow("Welcome to firmfact."), account.Name)
	refreshToolsQuietly(ctx, app, c, true)

	if o.NoWait {
		app.printNextStep(stepWorkspaces)
		return nil
	}
	start := time.Now()
	w := setupWait{workspace: Workspace{ID: account.ID, Name: account.Name}, timeout: o.WaitTimeout, out: app.Out, signup: true}
	if _, err := waitForSetup(ctx, app, c, w); err != nil {
		return err
	}
	timer.step("demo workspace setup", start)
	timer.total()
	app.printNextStep(stepAnalyzeSpend)
	return nil
}

// startSignup asks for the details, sends them and has the code emailed.
// It reports whether this run goes on to ask for the code: in a script the
// run ends here, successfully, as the code arrives by email and a second
// run gives it. A signup held for review ends here too.
func startSignup(ctx context.Context, app *App, c *api.Client, o *signupOptions, interactive bool, timer *stepTimer) (codeSent bool, err error) {
	lang := o.Language
	if lang == "" {
		lang = "en"
	}
	var choices struct {
		Data signupChoices `json:"data"`
	}
	if err := c.JSON(ctx, http.MethodGet, "/api/v1/signup/options?locale="+url.QueryEscape(lang), nil, &choices, false); err != nil {
		return false, err
	}
	opts := choices.Data

	if err := askSignupDetails(ctx, app, o, &opts, interactive); err != nil {
		return false, err
	}

	start := time.Now()
	var created struct {
		Status string `json:"status"`
	}
	body := map[string]any{"user": map[string]any{
		"name":               o.Name,
		"email":              o.Email,
		"password":           o.Password,
		"accept_terms":       o.AcceptTerms,
		"legal_entity_name":  o.Org,
		"organization_type":  o.Type,
		"base_currency":      o.Currency,
		"preferred_language": o.Language,
	}}
	if err := c.JSON(ctx, http.MethodPost, "/api/v1/signup", body, &created, false); err != nil {
		return false, err
	}
	timer.step("signup request", start)

	if created.Status == "held_for_review" {
		fmt.Fprintf(app.Out, "\nThanks. %s is not on our list of known work domains yet, so a person will\n"+
			"review your signup. We have emailed you and will let you know once it is approved.\n", domainOf(o.Email))
		return false, nil
	}

	fmt.Fprintf(app.Out, "\nIf %s can sign up, we have emailed it a 6-digit code (valid for 30 minutes).\n"+
		"Already have an account? Run `%s login` instead.\n\n", o.Email, app.Name)
	if !interactive {
		// Not a failure: the script did its part, and the next step is
		// the person's, reading the email.
		fmt.Fprintf(app.Out, "To finish, run `%s signup --email %s` with the code in %s, or piped to --code-stdin.\n",
			app.Name, shellWord(ui.SafeLine(o.Email), "<email>"), signupCodeEnv)
		return false, nil
	}
	return true, nil
}

func askSignupDetails(ctx context.Context, app *App, o *signupOptions, opts *signupChoices, interactive bool) error {
	var missing []string
	ask := func(value *string, flag, label, def string) error {
		if *value != "" {
			return nil
		}
		if !interactive {
			if def != "" {
				*value = def
				return nil
			}
			missing = append(missing, "--"+flag)
			return nil
		}
		answer, err := app.Prompt(ctx, label, def)
		*value = answer
		return err
	}

	if err := ask(&o.Email, "email", "Work email", ""); err != nil {
		return err
	}
	if err := ask(&o.Name, "name", "Your name", ""); err != nil {
		return err
	}
	if err := ask(&o.Org, "org", "Organisation (legal name)", orgFromEmail(o.Email)); err != nil {
		return err
	}

	if o.Type == "" && interactive {
		fmt.Fprintln(app.Out, "Organisation type (picks the sample data in your Demo workspace):")
		for i, t := range opts.OrganizationTypes {
			fmt.Fprintf(app.Out, "  %d) %s\n", i+1, ui.SafeLine(t.Label))
		}
		answer, err := app.Prompt(ctx, "Choose", "1")
		if err != nil {
			return err
		}
		o.Type = pickByNumberOrValue(answer, opts.OrganizationTypes)
	}
	if o.Type == "" {
		missing = append(missing, "--type")
	} else if !validType(o.Type, opts) {
		return usageErrorf("unknown organisation type %q (choose from: %s)", o.Type, typeValues(opts))
	}

	if err := ask(&o.Currency, "currency", "Base currency", opts.DefaultCurrency); err != nil {
		return err
	}
	o.Currency = strings.ToUpper(o.Currency)
	if err := ask(&o.Language, "language", "Interface language", "en"); err != nil {
		return err
	}

	if len(missing) > 0 {
		return usageErrorf("missing %s (or run in a terminal to be asked)", strings.Join(missing, ", "))
	}

	if o.Password == "" && !o.NoPassword && interactive {
		fmt.Fprintln(app.Out, "A password lets you also sign in on the web. Leave it empty to skip;")
		fmt.Fprintln(app.Out, "you can set one later with \"Forgot password\".")
		pw, err := app.PromptSecret(ctx, "Password (min. 12 characters, upper and lower case, a digit)")
		if err != nil {
			return err
		}
		if pw != "" {
			again, err := app.PromptSecret(ctx, "Repeat password")
			if err != nil {
				return err
			}
			if again != pw {
				return errors.New("the passwords do not match")
			}
		}
		o.Password = pw
	}

	if !o.AcceptTerms {
		fmt.Fprintln(app.Out, "\nTo sign up you agree to:")
		for _, t := range opts.Terms {
			fmt.Fprintf(app.Out, "  - %s: %s\n", ui.SafeLine(t.Title), ui.SafeLine(t.URL))
		}
		if !interactive {
			return usageErrorf("pass --accept-terms to accept the documents above")
		}
		ok, err := app.Confirm(ctx, "Do you accept these documents?")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("signup cancelled: the documents were not accepted")
		}
		o.AcceptTerms = true
	}
	return nil
}

type confirmAnswer struct {
	Status         string `json:"status"`
	Reason         string `json:"reason"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	ExpiresIn      int    `json:"expires_in"`
	DefaultAccount *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"default_account"`
}

type signupAccount struct{ ID, Name string }

// confirmSignup confirms the emailed code, the one given or, on a
// terminal, one it asks for; there a wrong code may be typed again.
func confirmSignup(ctx context.Context, app *App, c *api.Client, o *signupOptions, interactive bool, timer *stepTimer) (*api.TokenResponse, *signupAccount, error) {
	attempts := 1
	if interactive {
		attempts = 3
	}
	given := strings.TrimSpace(o.Code)
	for i := 0; i < attempts; i++ {
		code := given
		if i > 0 || code == "" {
			if !interactive {
				return nil, nil, usageErrorf("give the emailed code in %s or with --code-stdin", signupCodeEnv)
			}
			var err error
			if code, err = app.Prompt(ctx, "6-digit code from the email", ""); err != nil {
				return nil, nil, err
			}
		}

		start := time.Now()
		var ans confirmAnswer
		err := c.JSON(ctx, http.MethodPost, "/api/v1/signup/confirm",
			map[string]any{"user": map[string]any{"email": o.Email, "confirmation_code": code}}, &ans, false)
		var apiErr *api.Error
		invalid := errors.As(err, &apiErr) && apiErr.Code == "INVALID_CODE"
		if invalid && i+1 < attempts {
			fmt.Fprintln(app.Err, "That code did not work; check the email and try again.")
			continue
		}
		if invalid && given != "" && attempts == 1 {
			// The server says to run signup again, but a run with this code
			// would only confirm it again; a new code takes a run without.
			return nil, nil, fmt.Errorf("that code is invalid or has expired. For a new one, run `%s signup` with your details again, without the code (--code, --code-stdin or %s)",
				app.Name, signupCodeEnv)
		}
		if err != nil {
			return nil, nil, err
		}
		timer.step("code confirmation", start)

		switch {
		case ans.AccessToken != "" && ans.DefaultAccount != nil:
			tr := &api.TokenResponse{AccessToken: ans.AccessToken, RefreshToken: ans.RefreshToken, ExpiresIn: ans.ExpiresIn}
			if err := c.SetToken(tr.Token()); err != nil {
				return nil, nil, err
			}
			return tr, &signupAccount{ID: ans.DefaultAccount.ID, Name: ans.DefaultAccount.Name}, nil
		case ans.Reason == "browser_login_required":
			fmt.Fprintln(app.Out, "Your email is confirmed. Your organisation signs in with SSO or two-factor")
			fmt.Fprintf(app.Out, "authentication, so finish with `%s login` in a browser.\n", app.Name)
		default:
			fmt.Fprintf(app.Out, "Your email is confirmed. Finish setting up your workspace on the web, then run `%s login`.\n", app.Name)
		}
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("too many wrong codes; run `%s signup` again for a new one", app.Name)
}

type stepTimer struct {
	app     *App
	enabled bool
	started time.Time
}

func newStepTimer(app *App, enabled bool) *stepTimer {
	return &stepTimer{app: app, enabled: enabled, started: time.Now()}
}

func (t *stepTimer) step(name string, since time.Time) {
	if t.enabled {
		fmt.Fprintf(t.app.Err, "[timing] %-22s %s\n", name, time.Since(since).Round(10*time.Millisecond))
	}
}

func (t *stepTimer) total() {
	if t.enabled {
		fmt.Fprintf(t.app.Err, "[timing] %-22s %s (includes typing the code)\n", "total", time.Since(t.started).Round(10*time.Millisecond))
	}
}

func domainOf(email string) string {
	if _, domain, ok := strings.Cut(email, "@"); ok {
		return domain
	}
	return email
}

// orgFromEmail suggests an organisation name from the email domain, the same
// default the server uses: "acme-bank.com" -> "Acme Bank".
func orgFromEmail(email string) string {
	domain := domainOf(email)
	first, _, ok := strings.Cut(domain, ".")
	if !ok || first == "" {
		return ""
	}
	words := strings.Fields(strings.ReplaceAll(first, "-", " "))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func pickByNumberOrValue(answer string, types []struct {
	Value string `json:"value"`
	Label string `json:"label"`
}) string {
	for i, t := range types {
		if answer == fmt.Sprint(i+1) || strings.EqualFold(answer, t.Value) {
			return t.Value
		}
	}
	return answer
}

func validType(value string, opts *signupChoices) bool {
	for _, t := range opts.OrganizationTypes {
		if t.Value == value {
			return true
		}
	}
	return false
}

func typeValues(opts *signupChoices) string {
	values := make([]string, len(opts.OrganizationTypes))
	for i, t := range opts.OrganizationTypes {
		values[i] = t.Value
	}
	return strings.Join(values, ", ")
}
