package tui

import (
	"errors"
	"time"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/keyring"
	"github.com/aghman/meshbbs/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// Messages carrying async results back into Update.
type (
	statusMsg struct {
		text  string
		isErr bool
	}
	areasLoadedMsg struct{ areas []store.Area }
	postsLoadedMsg struct{ posts []store.Post }
	mailLoadedMsg  struct{ mail []store.DM }
	mailOpenedMsg  struct{ subject, body string }
	peersLoadedMsg struct{ peers []Peer }
	signupDoneMsg  struct{ nick, passphrase string }

	fileAreasLoadedMsg struct{ areas []store.Area }
	filesLoadedMsg     struct {
		area  string
		files []store.CatalogEntry
	}
)

func okf(text string) tea.Cmd { return func() tea.Msg { return statusMsg{text: text} } }
func errf(err error) tea.Cmd {
	return func() tea.Msg { return statusMsg{text: err.Error(), isErr: true} }
}
func errs(text string) tea.Cmd { return func() tea.Msg { return statusMsg{text: text, isErr: true} } }

func (m Model) loadAreas() tea.Cmd {
	return func() tea.Msg {
		areas, err := m.cfg.Store.ListAreas(m.ctx)
		if err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return areasLoadedMsg{areas: areas}
	}
}

// loadFileAreas fetches the file areas (§6.5).
//
// Separate from loadAreas because the two listings are separate: a message area
// and a file area are the same thing to the sync engine and different things to
// a person, and mixing them in one menu would be the second kind of confusion.
func (m Model) loadFileAreas() tea.Cmd {
	return func() tea.Msg {
		areas, err := m.cfg.Store.ListFileAreas(m.ctx)
		if err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return fileAreasLoadedMsg{areas: areas}
	}
}

// loadFiles reads what the whole NETWORK has in an area, not just what this
// node holds — that is the point of a replicating catalog (§6.5).
func (m Model) loadFiles(area string) tea.Cmd {
	return func() tea.Msg {
		files, err := m.cfg.Store.ListAreaContents(m.ctx, area)
		if err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return filesLoadedMsg{area: area, files: files}
	}
}

func (m Model) loadPosts(area string) tea.Cmd {
	return func() tea.Msg {
		posts, err := m.cfg.Store.ListPosts(m.ctx, area, 200)
		if err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return postsLoadedMsg{posts: posts}
	}
}

func (m Model) submitPost(area, subject, body string) tea.Cmd {
	return func() tea.Msg {
		if _, err := m.cfg.Service.Post(m.ctx, m.nick, area, subject, body); err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return statusMsg{text: "Posted to " + area + "."}
	}
}

func (m Model) loadMail() tea.Cmd {
	return func() tea.Msg {
		mail, err := m.cfg.Store.Inbox(m.ctx, m.nick, 100)
		if err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return mailLoadedMsg{mail: mail}
	}
}

// openMail decrypts a message.
//
// This is the only command that touches private key material, and it needs the
// session passphrase — which is why reading mail requires an unlock step that
// browsing does not (§8.2).
func (m Model) openMail(dm store.DM) tea.Cmd {
	return func() tea.Msg {
		payload, err := m.cfg.Service.OpenDM(m.ctx, m.nick, m.passphrase, dm.SealedBytes)
		if err != nil {
			return statusMsg{text: "Cannot open: " + err.Error(), isErr: true}
		}
		if err := m.cfg.Store.MarkDMRead(m.ctx, dm.ID); err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return mailOpenedMsg{subject: payload.Subject, body: payload.Text}
	}
}

func (m Model) sendMail(to, subject, body string) tea.Cmd {
	return func() tea.Msg {
		if _, err := m.cfg.Service.SendDM(m.ctx, m.nick, to, subject, body); err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return statusMsg{text: "Message sent to " + to + "."}
	}
}

func (m Model) loadPeers() tea.Cmd {
	return func() tea.Msg {
		if m.cfg.Presence == nil {
			return peersLoadedMsg{}
		}
		return peersLoadedMsg{peers: m.cfg.Presence.List()}
	}
}

// tryUnlock verifies the passphrase against the user's wrapped DM key.
func (m Model) tryUnlock(passphrase string) tea.Cmd {
	return func() tea.Msg {
		if err := m.cfg.Service.VerifyPassphrase(m.ctx, m.nick, passphrase); err != nil {
			if errors.Is(err, keyring.ErrWrongPassphrase) {
				return statusMsg{text: "Wrong passphrase.", isErr: true}
			}
			return statusMsg{text: err.Error(), isErr: true}
		}
		return unlockedMsg{passphrase: passphrase}
	}
}

// webCodeTTL is how long a passkey-enrolment code stays live.
//
// Short on purpose. The code is read off this terminal and typed into a browser
// that is usually on the same desk, so minutes are generous; the window is the
// only thing standing between a code left on a screen and somebody else's
// passkey on the account.
const webCodeTTL = 10 * time.Minute

type webCodeMsg struct {
	code    string
	expires int64
}

// issueWebCode mints a passkey-enrolment code for this session's account
// ([D18]).
//
// The authority this grants is narrow and must stay that way: the code
// registers a credential and cannot log anybody in. This session already proved
// account ownership, and the code carries exactly that proof to a browser —
// nothing more.
func (m Model) issueWebCode() tea.Cmd {
	return func() tea.Msg {
		if m.guest {
			return statusMsg{text: "Guests have no account to enrol. Register with `ssh new@` first.", isErr: true}
		}
		code, hash, err := auth.NewEnrolmentCode()
		if err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		expires := m.clockNow().Add(webCodeTTL).Unix()
		if err := m.cfg.Store.PutEnrolmentCode(m.ctx, m.nick, hash, expires); err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		m.cfg.Logger.Info("passkey enrolment code issued",
			"nick", m.nick, "remote", m.cfg.Remote, "expires_at", expires)
		return webCodeMsg{code: code, expires: expires}
	}
}

type unlockedMsg struct{ passphrase string }
type needKeySetupMsg struct{}
type needUnlockMsg struct{}

// checkMailAccess decides whether the user needs to create a message key or
// simply unlock an existing one.
//
// Accounts created by the CLI have no DM key, because the CLI cannot know a
// passphrase. Without this check they would hit "no DM key yet" with no way
// to act on it.
func (m Model) checkMailAccess() tea.Cmd {
	return func() tea.Msg {
		if _, err := m.cfg.Store.DMPublicKey(m.ctx, m.nick); err != nil {
			return needKeySetupMsg{}
		}
		return needUnlockMsg{}
	}
}

// createDMKey generates and wraps a message key for an account that has none.
func (m Model) createDMKey(passphrase string) tea.Cmd {
	return func() tea.Msg {
		if err := m.cfg.Service.EnsureDMKey(m.ctx, m.nick, passphrase); err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return unlockedMsg{passphrase: passphrase}
	}
}
