package namespace

import (
	"fmt"
	"time"

	"github.com/pingcap-incubator/weir/pkg/config"
	"github.com/pingcap-incubator/weir/pkg/proxy/driver"
	"github.com/pingcap-incubator/weir/pkg/util/sync2"
	"github.com/pingcap/errors"
)

type NamespaceManagerImpl struct {
	backends     *ToggleMapWrapper
	buildBackend BackendBuilder

	frontends     *ToggleMapWrapper
	buildFrontend FrontendBuilder

	users *sync2.Toggle
}

type FrontendBuilder func(cfg *config.FrontendNamespace) (driver.Frontend, error)
type BackendBuilder func(cfg *config.BackendNamespace) (driver.Backend, error)

func CreateNamespaceManagerImpl(
	cfgs []*config.Namespace, frontendBuilder FrontendBuilder, backendBuilder BackendBuilder,
	closeBackendDelay time.Duration, closeBackendFunc func(interface{})) (*NamespaceManagerImpl, error) {

	users, err := createUsers(cfgs)
	if err != nil {
		return nil, errors.WithMessage(ErrInitUsers, err.Error())
	}

	frontends, err := createFrontends(cfgs, frontendBuilder)
	if err != nil {
		return nil, errors.WithMessage(ErrInitFrontend, err.Error())
	}

	backends, err := createBackends(cfgs, backendBuilder, closeBackendDelay, closeBackendFunc)
	if err != nil {
		return nil, errors.WithMessage(ErrInitBackend, err.Error())
	}

	ns := &NamespaceManagerImpl{
		backends:      backends,
		frontends:     frontends,
		buildBackend:  backendBuilder,
		buildFrontend: frontendBuilder,
		users:         users,
	}
	return ns, nil
}

func createUsers(cfgs []*config.Namespace) (*sync2.Toggle, error) {
	users, err := CreateUserNamespaceMapper(cfgs)
	if err != nil {
		return nil, err
	}
	return sync2.NewToggle(users), nil
}

func createBackends(cfgs []*config.Namespace, buildBackend BackendBuilder,
	delay time.Duration, closeBackend func(interface{})) (*ToggleMapWrapper, error) {

	var err error
	backendValues := make(map[string]interface{})

	defer func() {
		if err != nil {
			for _, b := range backendValues {
				b.(driver.Backend).Close()
			}
		}
	}()

	for _, cfg := range cfgs {
		b, err2 := buildBackend(&cfg.Backend)
		if err2 != nil {
			err = fmt.Errorf("namespace: %v, err: %v", cfg.Namespace, err)
			return nil, err
		}
		backendValues[cfg.Namespace] = b
	}

	return NewToggleMapWrapper(backendValues, delay, closeBackend), nil
}

func createFrontends(cfgs []*config.Namespace, buildFrontend FrontendBuilder) (*ToggleMapWrapper, error) {
	frontendValues := make(map[string]interface{})

	for _, cfg := range cfgs {
		f, err := buildFrontend(&cfg.Frontend)
		if err != nil {
			return nil, fmt.Errorf("namespace: %v, err: %v", cfg.Namespace, err)
		}
		frontendValues[cfg.Namespace] = f
	}

	return NewToggleMapWrapperWithoutCloseFunc(frontendValues), nil
}

func (n *NamespaceManagerImpl) GetNamespace(username string) (string, bool) {
	return n.users.Current().(*UserNamespaceMapper).GetUserNamespace(username)
}

func (n *NamespaceManagerImpl) GetFrontend(namespace string) (driver.Frontend, bool) {
	i, ok := n.frontends.Get(namespace)
	if !ok {
		return nil, false
	}
	return i.(driver.Frontend), true
}

func (n *NamespaceManagerImpl) GetBackend(namespace string) (driver.Backend, bool) {
	i, ok := n.backends.Get(namespace)
	if !ok {
		return nil, false
	}
	return i.(driver.Backend), true
}

func (n *NamespaceManagerImpl) PrepareReloadBackend(namespace string, cfg *config.BackendNamespace) error {
	b, err := n.buildBackend(cfg)
	if err != nil {
		return err
	}

	if err := n.backends.ReloadPrepare(namespace, b); err != nil {
		b.Close()
		return err
	}

	return nil
}

func (n *NamespaceManagerImpl) CommitReloadBackend(namespace string) error {
	return n.backends.ReloadCommit(namespace)
}

func (n *NamespaceManagerImpl) PrepareReloadFrontend(namespace string, cfg *config.FrontendNamespace) error {
	f, err := n.buildFrontend(cfg)
	if err != nil {
		return err
	}

	if err := n.frontends.ReloadPrepare(namespace, f); err != nil {
		return err
	}

	users := n.users.Current().(*UserNamespaceMapper).Clone()
	users.RemoveNamespaceUsers(namespace)
	if err := users.AddNamespaceUsers(namespace, cfg); err != nil {
		return err
	}
	n.users.SwapOther(users)

	return nil
}

// TODO: this may cause concurrent problem
func (n *NamespaceManagerImpl) CommitReloadFrontend(namespace string) error {
	if err := n.frontends.ReloadCommit(namespace); err != nil {
		return err
	}

	if err := n.users.Toggle(); err != nil {
		return err
	}

	return nil
}

func (n *NamespaceManagerImpl) CreateNamespace(cfg *config.Namespace) error {
	ns := cfg.Namespace
	if _, ok := n.frontends.Get(ns); ok {
		return ErrDuplicatedNamespace
	}

	users := n.users.Current().(*UserNamespaceMapper).Clone()
	if err := users.AddNamespaceUsers(cfg.Namespace, &cfg.Frontend); err != nil {
		return err
	}
	n.users.SwapOther(users)

	fe, err := n.buildFrontend(&cfg.Frontend)
	if err != nil {
		return errors.WithMessage(ErrInitFrontend, err.Error())
	}

	be, err := n.buildBackend(&cfg.Backend)
	if err != nil {
		return errors.WithMessage(ErrInitBackend, err.Error())
	}

	var errCommit error
	defer func() {
		if errCommit != nil {
			be.Close()
		}
	}()

	if errCommit = n.frontends.Add(ns, fe); errCommit != nil {
		return errCommit
	}

	if errCommit = n.backends.Add(ns, be); errCommit != nil {
		return errCommit
	}

	// The users must be loaded at last, waiting for all namespace resources are ready.
	if errCommit = n.users.Toggle(); errCommit != nil {
		return errCommit
	}

	return nil
}

func (n *NamespaceManagerImpl) RemoveNamespace(name string) error {
	var errStr string
	users := n.users.Current().(*UserNamespaceMapper).Clone()
	users.RemoveNamespaceUsers(name)
	n.users.SwapOther(users)

	if err := n.users.Toggle(); err != nil {
		errStr = err.Error()
	}

	if err := n.frontends.Remove(name); err != nil {
		errStr += err.Error()
	}

	if err := n.backends.Remove(name); err != nil {
		errStr += err.Error()
	}

	if errStr != "" {
		return errors.New(errStr)
	}
	return nil
}
