package governor

type KeyGenenerator interface {
	GetKey() (string, error)
	GetParts(key string) ([]string, error)
}
