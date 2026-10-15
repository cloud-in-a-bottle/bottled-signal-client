package connector

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

const (
	FlowRegister = "register"
	FlowExisting = "existing"

	captchaURL = "https://signalcaptchas.org/registration/generate.html"
)

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

func (c *Connector) GetLoginFlows() []bridgev2.LoginFlow {
	flows := []bridgev2.LoginFlow{{
		ID:   FlowRegister,
		Name: "Register a phone number",
		Description: "This server becomes the number's primary Signal device. Whatever phone is the primary for it now " +
			"is signed out.",
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if accounts, err := c.RPC.ListAccounts(ctx); err == nil && len(accounts) > 0 {
		flows = append(flows, bridgev2.LoginFlow{
			ID:          FlowExisting,
			Name:        "Use the account already on this server",
			Description: "signal-cli here is still registered as " + accounts[0].Number + ".",
		})
	}
	return flows
}

func (c *Connector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case FlowRegister:
		return &registerLogin{c: c, user: user}, nil
	case FlowExisting:
		return &existingLogin{c: c, user: user}, nil
	}
	return nil, fmt.Errorf("unknown login flow %q", flowID)
}

// finishLogin makes the bridge's login for an account signal-cli has registered.
func (c *Connector) finishLogin(ctx context.Context, user *bridgev2.User, number string) (*bridgev2.LoginStep, error) {
	accounts, err := c.RPC.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	aci := ""
	for _, a := range accounts {
		if a.Number == number {
			aci = a.ACI
		}
	}
	if !signalid.IsACI(aci) {
		return nil, fmt.Errorf("signal-cli has no account for %s", number)
	}
	login, err := user.NewLogin(ctx, &database.UserLogin{
		ID:            signalid.LoginID(aci),
		RemoteName:    number,
		RemoteProfile: status.RemoteProfile{Phone: number},
		Metadata:      &UserLoginMetadata{Number: number},
	}, &bridgev2.NewLoginParams{DeleteOnConflict: true})
	if err != nil {
		return nil, err
	}
	go login.Client.Connect(login.Log.WithContext(c.br.BackgroundCtx))
	return &bridgev2.LoginStep{
		Type:           bridgev2.LoginStepTypeComplete,
		StepID:         "signal.complete",
		Instructions:   "Registered " + number + ".",
		CompleteParams: &bridgev2.LoginCompleteParams{UserLoginID: login.ID, UserLogin: login},
	}, nil
}

// registerLogin registers a number with signal-cli as its primary device: the number and how to send the code, a
// captcha if Signal wants one, the code, and the PIN if the number has registration lock.
type registerLogin struct {
	c      *Connector
	user   *bridgev2.User
	step   string
	number string
	voice  bool
	code   string
}

var _ bridgev2.LoginProcessUserInput = (*registerLogin)(nil)

const (
	stepNumber  = "signal.number"
	stepCaptcha = "signal.captcha"
	stepCode    = "signal.code"
	stepPIN     = "signal.pin"
)

func (l *registerLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return l.to(stepNumber), nil
}

func (l *registerLogin) Cancel() {}

func (l *registerLogin) to(step string) *bridgev2.LoginStep {
	l.step = step
	s := &bridgev2.LoginStep{Type: bridgev2.LoginStepTypeUserInput, StepID: step, UserInputParams: &bridgev2.LoginUserInputParams{}}
	switch step {
	case stepNumber:
		s.Instructions = "Registering signs out whatever phone is this number's primary Signal device now. You can " +
			"link your phone to this server afterwards, in settings."
		s.UserInputParams.Fields = []bridgev2.LoginInputDataField{
			{Type: bridgev2.LoginInputFieldTypePhoneNumber, ID: "number", Name: "Phone number",
				Description: "In international format, eg +15551234567"},
			{Type: bridgev2.LoginInputFieldTypeSelect, ID: "method", Name: "Send the code by",
				Options: []string{"Text message", "Voice call"}},
		}
	case stepCaptcha:
		s.Instructions = "Signal wants a captcha solved first. Open " + captchaURL + ", solve it, then copy the link " +
			"behind \"Open Signal\" (right-click it) and paste it here."
		s.UserInputParams.Fields = []bridgev2.LoginInputDataField{
			{Type: bridgev2.LoginInputFieldTypeToken, ID: "captcha", Name: "Captcha link", Description: "Starts with signalcaptcha://"},
		}
	case stepCode:
		s.Instructions = "Signal sent a code to " + l.number + "."
		s.UserInputParams.Fields = []bridgev2.LoginInputDataField{
			{Type: bridgev2.LoginInputFieldType2FACode, ID: "code", Name: "Verification code"},
		}
	case stepPIN:
		s.Instructions = l.number + " has registration lock on: enter its Signal PIN."
		s.UserInputParams.Fields = []bridgev2.LoginInputDataField{
			{Type: bridgev2.LoginInputFieldTypePassword, ID: "pin", Name: "Signal PIN"},
		}
	}
	return s
}

func (l *registerLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	l.c.loginMu.Lock()
	defer l.c.loginMu.Unlock()
	switch l.step {
	case stepNumber:
		number := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(input["number"])
		if !e164.MatchString(number) {
			return nil, errors.New("enter the number with its country code, eg +15551234567")
		}
		l.number, l.voice = number, input["method"] == "Voice call"
		return l.register(ctx, "")
	case stepCaptcha:
		captcha := strings.TrimSpace(input["captcha"])
		if !strings.HasPrefix(captcha, "signalcaptcha://") {
			return nil, errors.New("that isn't a captcha link: it should start with signalcaptcha://")
		}
		return l.register(ctx, captcha)
	case stepCode:
		l.code = strings.NewReplacer("-", "", " ", "").Replace(input["code"])
		return l.verify(ctx, "")
	case stepPIN:
		return l.verify(ctx, strings.TrimSpace(input["pin"]))
	}
	return nil, fmt.Errorf("unexpected login step %s", l.step)
}

func (l *registerLogin) register(ctx context.Context, captcha string) (*bridgev2.LoginStep, error) {
	err := signalcli.Register(ctx, l.c.Binary, l.c.DataDir, l.number, captcha, l.voice)
	if errors.Is(err, signalcli.ErrCaptchaRequired) {
		return l.to(stepCaptcha), nil
	} else if err != nil {
		return nil, err
	}
	return l.to(stepCode), nil
}

func (l *registerLogin) verify(ctx context.Context, pin string) (*bridgev2.LoginStep, error) {
	err := l.c.RPC.Verify(ctx, l.number, l.code, pin)
	if pin == "" && signalcli.IsRegistrationLocked(err) {
		return l.to(stepPIN), nil
	} else if err != nil {
		return nil, err
	}
	return l.c.finishLogin(ctx, l.user, l.number)
}

// existingLogin takes over an account signal-cli already has, eg after logging out here, or from before the bridge
// kept its own login.
type existingLogin struct {
	c    *Connector
	user *bridgev2.User
}

var _ bridgev2.LoginProcessUserInput = (*existingLogin)(nil)

func (l *existingLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	accounts, err := l.c.RPC.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	switch len(accounts) {
	case 0:
		return nil, errors.New("signal-cli has no account here")
	case 1:
		return l.c.finishLogin(ctx, l.user, accounts[0].Number)
	}
	numbers := make([]string, len(accounts))
	for i, a := range accounts {
		numbers[i] = a.Number
	}
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "signal.existing",
		Instructions: "signal-cli has several accounts here.",
		UserInputParams: &bridgev2.LoginUserInputParams{Fields: []bridgev2.LoginInputDataField{
			{Type: bridgev2.LoginInputFieldTypeSelect, ID: "number", Name: "Account", Options: numbers},
		}},
	}, nil
}

func (l *existingLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	return l.c.finishLogin(ctx, l.user, input["number"])
}

func (l *existingLogin) Cancel() {}
