package dict

// Dictionary describes the operations supported by every concurrent map in this module.
type Dictionary interface {
	Set(key string, value any)
	Get(key string) (any, bool)
	Delete(key string) bool
	Len() int
	Range(fn func(key string, value any) bool)
	Name() string
}
