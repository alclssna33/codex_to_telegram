//go:build !windows

package secrets

type unsupportedStore struct{}

func NewStore() Store { return unsupportedStore{} }

func (unsupportedStore) Read(string) ([]byte, error) { return nil, ErrUnsupported }
func (unsupportedStore) Write(string, []byte) error  { return ErrUnsupported }
func (unsupportedStore) Delete(string) error         { return ErrUnsupported }
