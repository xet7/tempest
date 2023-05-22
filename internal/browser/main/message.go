package browsermain

import (
	"context"
	"errors"
	"strings"
	"syscall/js"

	"zenhack.net/go/jsapi/streams"
	"zenhack.net/go/tea"
	"zenhack.net/go/tempest/capnp/collection"
	"zenhack.net/go/tempest/capnp/external"
	"zenhack.net/go/tempest/capnp/util"
	"zenhack.net/go/tempest/internal/common/types"
	"zenhack.net/go/tempest/pkg/exp/util/bytestream"
	"zenhack.net/go/util/exn"
	"zenhack.net/go/util/maybe"
	"zenhack.net/go/util/orerr"
)

type Cmd = func(context.Context, func(Msg))

type Msg = tea.Message[Model]

type NewError struct {
	Err error
}

type UpsertGrain struct {
	ID    types.GrainID
	Grain Grain
}

type RemoveGrain struct {
	ID types.GrainID
}

type ClearGrains struct{}

type UpsertPackage struct {
	ID  types.ID[external.Package]
	Pkg external.Package
}

type RemovePackage struct {
	ID types.ID[external.Package]
}

type ClearPackages struct{}

type ChangeFocus struct {
	NewFocus Focus
}

type FocusGrain struct {
	ID types.GrainID
}

type SpawnGrain struct {
	Index int
	PkgID types.ID[external.Package]
}

type CloseGrain struct {
	ID types.GrainID
}

type ShareGrain struct {
	ID types.GrainID
}

type EditEmailLogin struct {
	NewValue string
}

type EditEmailToken struct {
	NewValue string
}

type SubmitEmailLogin struct {
}

type SubmitEmailToken struct {
}

type LoginSessionResult struct {
	Result orerr.OrErr[Sessions]
}

// The user has selected an spk file to upload & install
type NewAppPkgFile struct {
	Name   string
	Size   int
	Reader streams.ReadableStreamDefaultReader
}

// The URL has changed. We never send this one explicitly from UI code; instead
// we set up an event listener for the "hashchange" event in our main function.
// All of our URLs start with #, so this covers everything.
type Navigate struct {
	OldURL, NewURL string
}

func (msg NewError) Update(m Model) (Model, Cmd) {
	m.Errors = append(m.Errors, msg.Err)
	return m, nil
}

func (msg UpsertGrain) Update(m Model) (Model, Cmd) {
	m.Grains[msg.ID].Controller.Release()
	m.Grains[msg.ID] = msg.Grain
	return m, nil
}

func (msg RemoveGrain) Update(m Model) (Model, Cmd) {
	m.Grains[msg.ID].Controller.Release()
	delete(m.Grains, msg.ID)
	return m, nil
}

func (ClearGrains) Update(m Model) (Model, Cmd) {
	m.Grains = make(map[types.GrainID]Grain)
	return m, nil
}

func (msg UpsertPackage) Update(m Model) (Model, Cmd) {
	m.Packages[msg.ID].Controller().Release()
	m.Packages[msg.ID] = msg.Pkg
	return m, nil
}

func (msg RemovePackage) Update(m Model) (Model, Cmd) {
	// TODO(perf): release the whole message?
	m.Packages[msg.ID].Controller().Release()
	delete(m.Packages, msg.ID)
	return m, nil
}

func (ClearPackages) Update(m Model) (Model, Cmd) {
	m.Packages = make(map[types.ID[external.Package]]external.Package)
	return m, nil
}

func (msg ChangeFocus) Update(m Model) (Model, Cmd) {
	m.CurrentFocus = msg.NewFocus
	return m, nil
}

func (msg FocusGrain) Update(m Model) (Model, Cmd) {
	m.CurrentFocus = FocusOpenGrain
	m.FocusedGrain = msg.ID
	_, ok := m.OpenGrains[msg.ID]
	if !ok {
		index := m.GrainDomOrder.Add(msg.ID)
		m.OpenGrains[msg.ID] = OpenGrain{
			DomIndex: index,
		}
	}
	return m, nil
}

func (msg CloseGrain) Update(m Model) (Model, Cmd) {
	g, ok := m.OpenGrains[msg.ID]
	if ok {
		delete(m.OpenGrains, msg.ID)
		if m.CurrentFocus == FocusOpenGrain && m.FocusedGrain == msg.ID {
			m.CurrentFocus = FocusGrainList
			m.FocusedGrain = ""
		}
		m.GrainDomOrder.Remove(g.DomIndex)
	}
	return m, nil
}

func (msg SpawnGrain) Update(m Model) (Model, Cmd) {
	pkg := m.Packages[msg.PkgID]

	ctrl := pkg.Controller().AddRef()

	return m, func(ctx context.Context, sendMsg func(Msg)) {
		err := exn.Try0(func(throw func(error)) {

			defer ctrl.Release()
			fut, rel := ctrl.Create(ctx, func(p external.Package_Controller_create_Params) error {
				return exn.Try0(func(throw exn.Thrower) {
					manifest, err := pkg.Manifest()
					throw(err)
					appTitle, err := manifest.AppTitle()
					throw(err)
					appTitleText, err := appTitle.DefaultText()
					throw(err)

					actions, err := manifest.Actions()
					throw(err)
					nounPhrase, err := actions.At(msg.Index).NounPhrase()
					throw(err)
					nounPhraseText, err := nounPhrase.DefaultText()
					throw(err)

					p.SetTitle("Untitled " + appTitleText + " " + nounPhraseText)
					p.SetActionIndex(uint32(msg.Index))
				})
			})
			defer rel()
			res, err := fut.Struct()
			throw(err)

			id, err := res.Id()
			throw(err)
			view, err := res.View()

			title, err := view.Title()
			throw(err)
			sessionToken, err := view.SessionToken()
			throw(err)

			sendMsg(UpsertGrain{
				ID: types.GrainID(id),
				Grain: Grain{
					Title:        title,
					SessionToken: sessionToken,
					Controller:   view.Controller().AddRef(),
				},
			})
			sendMsg(FocusGrain{ID: types.GrainID(id)})
		})
		if err != nil {
			sendMsg(NewError{Err: err})
		}
	}
}

func (msg LoginSessionResult) Update(m Model) (Model, Cmd) {
	m.LoginSessions = maybe.New(msg.Result)
	sess, err := msg.Result.Get()
	if err != nil {
		return m, nil
	}
	return m, func(ctx context.Context, sendMsg func(Msg)) {
		// TODO: there's no actual reason to wait for the result before doing all this:
		pusher := collection.Pusher_ServerToClient(pusher[types.ID[external.Package], external.Package]{
			sendMsg: sendMsg,
			hooks:   pkgPusher{},
		})
		ret, rel := sess.User.ListPackages(context.Background(), func(p external.UserSession_listPackages_Params) error {
			p.SetInto(pusher)
			return nil
		})
		defer rel()
		_, err := ret.Struct()
		if err != nil {
			println("listPackages(): " + err.Error())
		}
	}
}

func (msg EditEmailLogin) Update(m Model) (Model, Cmd) {
	m.LoginForm.EmailInput = msg.NewValue
	return m, nil
}

func (msg EditEmailToken) Update(m Model) (Model, Cmd) {
	m.LoginForm.TokenInput = msg.NewValue
	return m, nil
}

func (msg SubmitEmailLogin) Update(m Model) (Model, Cmd) {
	api := m.API
	address := m.LoginForm.EmailInput
	m.LoginForm.TokenSent = true
	m.LoginForm.EmailInput = ""
	return m, func(ctx context.Context, sendMsg func(Msg)) {
		authFut, rel := api.Authenticator(ctx, nil)
		defer rel()
		sendFut, rel := authFut.Authenticator().
			SendEmailAuthToken(ctx, func(p external.Authenticator_sendEmailAuthToken_Params) error {
				return p.SetAddress(address)
			})
		if _, err := sendFut.Struct(); err != nil {
			sendMsg(NewError{Err: err})
		}
	}
}

func (msg SubmitEmailToken) Update(m Model) (Model, Cmd) {
	return m, func(context.Context, func(Msg)) {
		js.Global().Get("location").Set("href", "/login/email/"+strings.TrimSpace(m.LoginForm.TokenInput))
	}
}

func (msg NewAppPkgFile) Update(m Model) (Model, Cmd) {
	var (
		userSess external.UserSession
		err      error
	)
	res, ok := m.LoginSessions.Get()
	if ok {
		login, err := res.Get()
		if err == nil {
			userSess = login.User.AddRef()
		}
	}
	return m, func(ctx context.Context, sendMsg func(Msg)) {
		if !ok {
			sendMsg(NewError{
				Err: errors.New("No login session yet; can't install app"),
			})
			return
		}
		if err != nil {
			sendMsg(NewError{Err: err})
			return
		}
		defer userSess.Release()
		c := js.Global().Get("console")
		c.Call("log", msg.Name, msg.Size, msg.Reader.Value)
		ipFut, rel := userSess.InstallPackage(ctx, nil)
		defer rel()
		stream := ipFut.Stream()
		pkgFut, rel := stream.GetPackage(ctx, nil)
		defer rel()
		wc := bytestream.ToWriteCloser(ctx, util.ByteStream(stream))
		_, err := msg.Reader.WriteTo(wc)
		if err != nil {
			sendMsg(NewError{Err: err})
			return
		}
		err = wc.Close()
		if err != nil {
			sendMsg(NewError{Err: err})
			return
		}
		pkgRes, err := pkgFut.Struct()
		if err != nil {
			sendMsg(NewError{Err: err})
			return
		}
		pkgId, err := pkgRes.Id()
		if err != nil {
			sendMsg(NewError{Err: err})
		}
		pkg, err := pkgRes.Package()
		if err != nil {
			sendMsg(NewError{Err: err})
			return
		}
		pkg, err = cloneStruct(pkg)
		if err != nil {
			sendMsg(NewError{Err: err})
			return
		}
		sendMsg(UpsertPackage{
			ID:  types.ID[external.Package](pkgId),
			Pkg: pkg,
		})

	}
}

func (msg ShareGrain) Update(m Model) (Model, Cmd) {
	// TODO: present a UI of some kind; right now we just fetch the token
	// and then log it.
	ctrl := m.Grains[msg.ID].Controller.AddRef()
	return m, func(ctx context.Context, sendMsg func(Msg)) {
		defer ctrl.Release()
		fut, rel := ctrl.MakeSharingToken(
			ctx,
			func(p external.UiView_Controller_makeSharingToken_Params) error {
				// TODO: fill in permissions and note.
				return nil

			},
		)
		defer rel()
		res, err := fut.Struct()
		if err != nil {
			sendMsg(NewError{Err: err})
			return
		}
		token, err := res.Token()
		if err != nil {
			sendMsg(NewError{Err: err})
			return
		}
		js.Global().Get("console").Call("log", "token: "+token)
	}
}

func (msg Navigate) Update(m Model) (Model, Cmd) {
	// TODO
	println("old URL: " + msg.OldURL)
	println("new URL: " + msg.NewURL)
	return m, nil
}
