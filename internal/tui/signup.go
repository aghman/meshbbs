package tui

import (
	"strings"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// signupStep is the position in the registration flow.
type signupStep int

const (
	stepNick signupStep = iota
	stepPassword
	stepPasswordConfirm
	stepPassphraseChoice
	stepPassphrase
	stepPassphraseConfirm
	stepAcknowledge
	stepDone
)

// signupState drives registration (§6.7).
type signupState struct {
	step    signupStep
	hasKey  bool // an SSH key was offered, so a password is optional
	nick    textInput
	pass    textInput
	pass2   textInput
	phrase  textInput
	phrase2 textInput

	samePassphrase bool
	acknowledged   bool
	err            string
}

func newSignupState(suggested string, hasKey bool) signupState {
	s := signupState{
		hasKey:  hasKey,
		nick:    newInput("Nick: ", 16, false),
		pass:    newInput("Password: ", 64, true),
		pass2:   newInput("Confirm: ", 64, true),
		phrase:  newInput("DM passphrase: ", 64, true),
		phrase2: newInput("Confirm: ", 64, true),
	}
	s.nick.value = suggested
	return s
}

// handleSignupKey advances the registration flow.
func (m Model) handleSignupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := m.signup

	if msg.Type == tea.KeyEsc {
		return m.leave()
	}

	switch s.step {
	case stepNick:
		if msg.Type == tea.KeyEnter {
			nick := strings.TrimSpace(s.nick.String())
			if err := store.ValidateNick(nick); err != nil {
				s.err = err.Error()
				m.signup = s
				return m, nil
			}
			if _, err := m.cfg.Store.GetUser(m.ctx, nick); err == nil {
				s.err = "That nick is already taken here. Nicks are local to this BBS, so pick another."
				m.signup = s
				return m, nil
			}
			s.err = ""
			// With a key already offered, a password is optional — SSH handed
			// us the credential, so registration can be a single step.
			s.step = stepPassword
			m.signup = s
			return m, nil
		}
		s.nick, _ = s.nick.Update(msg)
		m.signup = s
		return m, nil

	case stepPassword:
		if msg.Type == tea.KeyEnter {
			pw := s.pass.String()
			if pw == "" {
				if s.hasKey {
					// Key-only account: skip straight to the DM passphrase,
					// which is still required because it protects mail.
					s.err = ""
					s.step = stepPassphrase
					s.samePassphrase = false
					m.signup = s
					return m, nil
				}
				s.err = "A password is required when you connect without an SSH key."
				m.signup = s
				return m, nil
			}
			if len(pw) < 8 {
				s.err = "Password must be at least 8 characters."
				m.signup = s
				return m, nil
			}
			s.err = ""
			s.step = stepPasswordConfirm
			m.signup = s
			return m, nil
		}
		s.pass, _ = s.pass.Update(msg)
		m.signup = s
		return m, nil

	case stepPasswordConfirm:
		if msg.Type == tea.KeyEnter {
			if s.pass.String() != s.pass2.String() {
				s.err = "Passwords do not match."
				s.pass2.Clear()
				m.signup = s
				return m, nil
			}
			s.err = ""
			s.step = stepPassphraseChoice
			m.signup = s
			return m, nil
		}
		s.pass2, _ = s.pass2.Update(msg)
		m.signup = s
		return m, nil

	case stepPassphraseChoice:
		switch strings.ToLower(msg.String()) {
		case "y", "enter":
			s.samePassphrase = true
			s.step = stepAcknowledge
		case "n":
			s.samePassphrase = false
			s.step = stepPassphrase
		}
		m.signup = s
		return m, nil

	case stepPassphrase:
		if msg.Type == tea.KeyEnter {
			if len(s.phrase.String()) < 8 {
				s.err = "Passphrase must be at least 8 characters."
				m.signup = s
				return m, nil
			}
			s.err = ""
			s.step = stepPassphraseConfirm
			m.signup = s
			return m, nil
		}
		s.phrase, _ = s.phrase.Update(msg)
		m.signup = s
		return m, nil

	case stepPassphraseConfirm:
		if msg.Type == tea.KeyEnter {
			if s.phrase.String() != s.phrase2.String() {
				s.err = "Passphrases do not match."
				s.phrase2.Clear()
				m.signup = s
				return m, nil
			}
			s.err = ""
			s.step = stepAcknowledge
			m.signup = s
			return m, nil
		}
		s.phrase2, _ = s.phrase2.Update(msg)
		m.signup = s
		return m, nil

	case stepAcknowledge:
		if strings.ToLower(msg.String()) == "y" {
			s.acknowledged = true
			m.signup = s
			return m, m.completeSignup()
		}
		if strings.ToLower(msg.String()) == "n" {
			return m.leave()
		}
		return m, nil
	}
	return m, nil
}

// completeSignup creates the account.
func (m Model) completeSignup() tea.Cmd {
	s := m.signup
	nick := strings.TrimSpace(s.nick.String())
	password := s.pass.String()

	passphrase := s.phrase.String()
	if s.samePassphrase {
		passphrase = password
	}

	return func() tea.Msg {
		var hash string
		if password != "" {
			h, err := auth.HashPassword(password)
			if err != nil {
				return statusMsg{text: err.Error(), isErr: true}
			}
			hash = h
		}

		// Note the capabilities: DefaultCapabilities deliberately EXCLUDES
		// post_federated ([N7]). The door is open, the commons is gated.
		if _, err := m.cfg.Store.CreateUser(m.ctx, store.CreateUserOptions{
			Nick:         nick,
			PasswordHash: hash,
			CanLogin:     true,
			Capabilities: store.DefaultCapabilities,
		}); err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}

		// Enrol the key SSH already handed us, so the next login is
		// passwordless with no key-pasting step (§5.1).
		if m.cfg.PublicKey != "" {
			if err := m.cfg.Store.AddUserKey(m.ctx, nick,
				m.cfg.PublicKey, m.cfg.KeyFP, "enrolled at signup"); err != nil {
				return statusMsg{text: err.Error(), isErr: true}
			}
		}

		if err := m.cfg.Service.EnsureDMKey(m.ctx, nick, passphrase); err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		if err := m.cfg.Store.Audit(m.ctx, nick, "user.register", nick, ""); err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return signupDoneMsg{nick: nick, passphrase: passphrase}
	}
}
