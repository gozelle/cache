package cache

import (
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"
)

type Item[T any] struct {
	Object     T
	Expiration int64
}

// Expired Returns true if the item has expired.
func (item Item[T]) Expired() bool {
	if item.Expiration == 0 {
		return false
	}
	return time.Now().UnixNano() > item.Expiration
}

const (
	// NoExpiration For use with functions that take an expiration time.
	NoExpiration time.Duration = -1
	// DefaultExpiration For use with functions that take an expiration time. Equivalent to
	// passing in the same expiration duration as was given to New() or
	// NewFrom() when the cache was created (e.g. 5 minutes.)
	DefaultExpiration time.Duration = 0
)

type Cache[T any] struct {
	*cache[T]
	// If this is confusing, see the comment at the bottom of New()
}

type cache[T any] struct {
	defaultExpiration time.Duration
	items             map[string]Item[T]
	mu                sync.RWMutex
	onEvicted         func(string, interface{})
	janitor           *janitor[T]
}

// Set Add an item to the cache, replacing any existing item. If the duration is 0
// (DefaultExpiration), the cache's default expiration time is used. If it is -1
// (NoExpiration), the item never expires.
func (c *cache[T]) Set(k string, x T, d time.Duration) {
	// "Inlining" of set
	var e int64
	if d == DefaultExpiration {
		d = c.defaultExpiration
	}
	if d > 0 {
		e = time.Now().Add(d).UnixNano()
	}
	c.mu.Lock()
	c.items[k] = Item[T]{
		Object:     x,
		Expiration: e,
	}
	// TODO: Calls to mu.Unlock are currently not deferred because defer
	// adds ~200 ns (as of go1.)
	c.mu.Unlock()
}

func (c *cache[T]) set(k string, x T, d time.Duration) {
	var e int64
	if d == DefaultExpiration {
		d = c.defaultExpiration
	}
	if d > 0 {
		e = time.Now().Add(d).UnixNano()
	}
	c.items[k] = Item[T]{
		Object:     x,
		Expiration: e,
	}
}

// SetDefault Add an item to the cache, replacing any existing item, using the default
// expiration.
func (c *cache[T]) SetDefault(k string, x T) {
	c.Set(k, x, DefaultExpiration)
}

// Add an item to the cache only if an item doesn't already exist for the given
// key, or if the existing item has expired. Returns an error otherwise.
func (c *cache[T]) Add(k string, x T, d time.Duration) error {
	c.mu.Lock()
	_, found := c.get(k)
	if found {
		c.mu.Unlock()
		return fmt.Errorf("item %s already exists", k)
	}
	c.set(k, x, d)
	c.mu.Unlock()
	return nil
}

// Replace Set a new value for the cache key only if it already exists, and the existing
// item hasn't expired. Returns an error otherwise.
func (c *cache[T]) Replace(k string, x T, d time.Duration) error {
	c.mu.Lock()
	_, found := c.get(k)
	if !found {
		c.mu.Unlock()
		return fmt.Errorf("item %s doesn't exist", k)
	}
	c.set(k, x, d)
	c.mu.Unlock()
	return nil
}

// Get an item from the cache. Returns the item or nil, and a bool indicating
// whether the key was found.
func (c *cache[T]) Get(k string) (r T, ok bool) {
	c.mu.RLock()
	// "Inlining" of get and Expired
	item, found := c.items[k]
	if !found {
		c.mu.RUnlock()
		return
	}
	if item.Expiration > 0 {
		if time.Now().UnixNano() > item.Expiration {
			c.mu.RUnlock()
			return
		}
	}
	c.mu.RUnlock()
	r = item.Object
	ok = true
	return
}

// GetWithExpiration returns an item and its expiration time from the cache.
// It returns the item or nil, the expiration time if one is set (if the item
// never expires a zero value for time.Time is returned), and a bool indicating
// whether the key was found.
func (c *cache[T]) GetWithExpiration(k string) (r interface{}, t time.Time, ok bool) {
	c.mu.RLock()
	// "Inlining" of get and Expired
	item, found := c.items[k]
	if !found {
		c.mu.RUnlock()
		return
	}
	
	if item.Expiration > 0 {
		if time.Now().UnixNano() > item.Expiration {
			c.mu.RUnlock()
			return
		}
		
		// Return the item and the expiration time
		c.mu.RUnlock()
		r = item.Object
		t = time.Unix(0, item.Expiration)
		ok = true
		return
	}
	
	// If expiration <= 0 (i.e. no expiration time set) then return the item
	// and a zeroed time.Time
	c.mu.RUnlock()
	r = item.Object
	ok = true
	return
}

func (c *cache[T]) get(k string) (r T, ok bool) {
	item, found := c.items[k]
	if !found {
		return
	}
	// "Inlining" of Expired
	if item.Expiration > 0 {
		if time.Now().UnixNano() > item.Expiration {
			return
		}
	}
	r = item.Object
	ok = true
	return
}

// Increment an item of type int, int8, int16, int32, int64, uintptr, uint,
// uint8, uint32, or uint64, float32 or float64 by n. Returns an error if the
// item's value is not an integer, if it was not found, or if it is not
// possible to increment it by n. To retrieve the incremented value, use one
// of the specialized methods, e.g. IncrementInt64.
func (c *cache[T]) Increment(k string, n int64) error {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return fmt.Errorf("item %s not found", k)
	}
	
	switch any(v.Object).(type) {
	case int:
		v.Object = any(any(v.Object).(int) + int(n)).(T)
	case int8:
		v.Object = any(any(v.Object).(int8) + int8(n)).(T)
	case int16:
		v.Object = any(any(v.Object).(int16) + int16(n)).(T)
	case int32:
		v.Object = any(any(v.Object).(int32) + int32(n)).(T)
	case int64:
		v.Object = any(any(v.Object).(int64) + n).(T)
	case uint:
		v.Object = any(any(v.Object).(uint) + uint(n)).(T)
	case uintptr:
		v.Object = any(any(v.Object).(uintptr) + uintptr(n)).(T)
	case uint8:
		v.Object = any(any(v.Object).(uint8) + uint8(n)).(T)
	case uint16:
		v.Object = any(any(v.Object).(uint16) + uint16(n)).(T)
	case uint32:
		v.Object = any(any(v.Object).(uint32) + uint32(n)).(T)
	case uint64:
		v.Object = any(any(v.Object).(uint64) + uint64(n)).(T)
	case float32:
		v.Object = any(any(v.Object).(float32) + float32(n)).(T)
	case float64:
		v.Object = any(any(v.Object).(float64) + float64(n)).(T)
	default:
		c.mu.Unlock()
		return fmt.Errorf("the value for %s is not an integer", k)
	}
	c.items[k] = v
	c.mu.Unlock()
	return nil
}

// IncrementFloat Increment an item of type float32 or float64 by n. Returns an error if the
// item's value is not floating point, if it was not found, or if it is not
// possible to increment it by n. Pass a negative number to decrement the
// value. To retrieve the incremented value, use one of the specialized methods,
// e.g. IncrementFloat64.
func (c *cache[T]) IncrementFloat(k string, n float64) error {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return fmt.Errorf("item %s not found", k)
	}
	switch any(v.Object).(type) {
	case float32:
		v.Object = any(any(v.Object).(float32) + float32(n)).(T)
	case float64:
		v.Object = any(any(v.Object).(float64) + n).(T)
	default:
		c.mu.Unlock()
		return fmt.Errorf("the value for %s does not have type float32 or float64", k)
	}
	c.items[k] = v
	c.mu.Unlock()
	return nil
}

// IncrementInt Increment an item of type int by n. Returns an error if the item's value is
// not an int, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache[T]) IncrementInt(k string, n int) (int, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(int)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an int", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementInt8 Increment an item of type int8 by n. Returns an error if the item's value is
// not an int8, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache[T]) IncrementInt8(k string, n int8) (int8, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(int8)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an int8", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementInt16 Increment an item of type int16 by n. Returns an error if the item's value is
// not an int16, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache[T]) IncrementInt16(k string, n int16) (int16, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(int16)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an int16", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementInt32 Increment an item of type int32 by n. Returns an error if the item's value is
// not an int32, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache[T]) IncrementInt32(k string, n int32) (int32, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(int32)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an int32", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementInt64 Increment an item of type int64 by n. Returns an error if the item's value is
// not an int64, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache[T]) IncrementInt64(k string, n int64) (int64, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(int64)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an int64", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementUint Increment an item of type uint by n. Returns an error if the item's value is
// not an uint, or if it was not found. If there is no error, the incremented
// value is returned.
func (c *cache[T]) IncrementUint(k string, n uint) (uint, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uint)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uint", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementUintptr Increment an item of type uintptr by n. Returns an error if the item's value
// is not an uintptr, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache[T]) IncrementUintptr(k string, n uintptr) (uintptr, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uintptr)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uintptr", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementUint8 Increment an item of type uint8 by n. Returns an error if the item's value
// is not an uint8, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache[T]) IncrementUint8(k string, n uint8) (uint8, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uint8)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uint8", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementUint16 Increment an item of type uint16 by n. Returns an error if the item's value
// is not an uint16, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache[T]) IncrementUint16(k string, n uint16) (uint16, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uint16)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uint16", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementUint32 Increment an item of type uint32 by n. Returns an error if the item's value
// is not an uint32, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache[T]) IncrementUint32(k string, n uint32) (uint32, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uint32)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uint32", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementUint64 Increment an item of type uint64 by n. Returns an error if the item's value
// is not an uint64, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache[T]) IncrementUint64(k string, n uint64) (uint64, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uint64)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uint64", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementFloat32 Increment an item of type float32 by n. Returns an error if the item's value
// is not an float32, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache[T]) IncrementFloat32(k string, n float32) (float32, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(float32)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an float32", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// IncrementFloat64 Increment an item of type float64 by n. Returns an error if the item's value
// is not an float64, or if it was not found. If there is no error, the
// incremented value is returned.
func (c *cache[T]) IncrementFloat64(k string, n float64) (float64, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(float64)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an float64", k)
	}
	nv := rv + n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// Decrement an item of type int, int8, int16, int32, int64, uintptr, uint,
// uint8, uint32, or uint64, float32 or float64 by n. Returns an error if the
// item's value is not an integer, if it was not found, or if it is not
// possible to decrement it by n. To retrieve the decremented value, use one
// of the specialized methods, e.g. DecrementInt64.
func (c *cache[T]) Decrement(k string, n int64) error {
	// TODO: Implement Increment and Decrement more cleanly.
	// (Cannot do Increment(k, n*-1) for uints.)
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return fmt.Errorf("item not found")
	}
	switch any(v.Object).(type) {
	case int:
		v.Object = any(any(v.Object).(int) - int(n)).(T)
	case int8:
		v.Object = any(any(v.Object).(int8) - int8(n)).(T)
	case int16:
		v.Object = any(any(v.Object).(int16) - int16(n)).(T)
	case int32:
		v.Object = any(any(v.Object).(int32) - int32(n)).(T)
	case int64:
		v.Object = any(any(v.Object).(int64) - n).(T)
	case uint:
		v.Object = any(any(v.Object).(uint) - uint(n)).(T)
	case uintptr:
		v.Object = any(any(v.Object).(uintptr) - uintptr(n)).(T)
	case uint8:
		v.Object = any(any(v.Object).(uint8) - uint8(n)).(T)
	case uint16:
		v.Object = any(any(v.Object).(uint16) - uint16(n)).(T)
	case uint32:
		v.Object = any(any(v.Object).(uint32) - uint32(n)).(T)
	case uint64:
		v.Object = any(any(v.Object).(uint64) - uint64(n)).(T)
	case float32:
		v.Object = any(any(v.Object).(float32) - float32(n)).(T)
	case float64:
		v.Object = any(any(v.Object).(float64) - float64(n)).(T)
	default:
		c.mu.Unlock()
		return fmt.Errorf("the value for %s is not an integer", k)
	}
	c.items[k] = v
	c.mu.Unlock()
	return nil
}

// DecrementFloat Decrement an item of type float32 or float64 by n. Returns an error if the
// item's value is not floating point, if it was not found, or if it is not
// possible to decrement it by n. Pass a negative number to decrement the
// value. To retrieve the decremented value, use one of the specialized methods,
// e.g. DecrementFloat64.
func (c *cache[T]) DecrementFloat(k string, n float64) error {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return fmt.Errorf("item %s not found", k)
	}
	switch any(v.Object).(type) {
	case float32:
		v.Object = any(any(v.Object).(float32) - float32(n)).(T)
	case float64:
		v.Object = any(any(v.Object).(float64) - n).(T)
	default:
		c.mu.Unlock()
		return fmt.Errorf("the value for %s does not have type float32 or float64", k)
	}
	c.items[k] = v
	c.mu.Unlock()
	return nil
}

// DecrementInt Decrement an item of type int by n. Returns an error if the item's value is
// not an int, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache[T]) DecrementInt(k string, n int) (int, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(int)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an int", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementInt8 Decrement an item of type int8 by n. Returns an error if the item's value is
// not an int8, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache[T]) DecrementInt8(k string, n int8) (int8, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(int8)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an int8", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementInt16 Decrement an item of type int16 by n. Returns an error if the item's value is
// not an int16, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache[T]) DecrementInt16(k string, n int16) (int16, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(int16)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an int16", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementInt32 Decrement an item of type int32 by n. Returns an error if the item's value is
// not an int32, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache[T]) DecrementInt32(k string, n int32) (int32, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(int32)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an int32", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementInt64 Decrement an item of type int64 by n. Returns an error if the item's value is
// not an int64, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache[T]) DecrementInt64(k string, n int64) (int64, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(int64)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an int64", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementUint Decrement an item of type uint by n. Returns an error if the item's value is
// not an uint, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache[T]) DecrementUint(k string, n uint) (uint, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uint)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uint", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementUintptr Decrement an item of type uintptr by n. Returns an error if the item's value
// is not an uintptr, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache[T]) DecrementUintptr(k string, n uintptr) (uintptr, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uintptr)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uintptr", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementUint8 Decrement an item of type uint8 by n. Returns an error if the item's value is
// not an uint8, or if it was not found. If there is no error, the decremented
// value is returned.
func (c *cache[T]) DecrementUint8(k string, n uint8) (uint8, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uint8)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uint8", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementUint16 Decrement an item of type uint16 by n. Returns an error if the item's value
// is not an uint16, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache[T]) DecrementUint16(k string, n uint16) (uint16, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uint16)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uint16", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementUint32 Decrement an item of type uint32 by n. Returns an error if the item's value
// is not an uint32, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache[T]) DecrementUint32(k string, n uint32) (uint32, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uint32)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uint32", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementUint64 Decrement an item of type uint64 by n. Returns an error if the item's value
// is not an uint64, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache[T]) DecrementUint64(k string, n uint64) (uint64, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(uint64)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an uint64", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementFloat32 Decrement an item of type float32 by n. Returns an error if the item's value
// is not an float32, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache[T]) DecrementFloat32(k string, n float32) (float32, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(float32)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an float32", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// DecrementFloat64 Decrement an item of type float64 by n. Returns an error if the item's value
// is not an float64, or if it was not found. If there is no error, the
// decremented value is returned.
func (c *cache[T]) DecrementFloat64(k string, n float64) (float64, error) {
	c.mu.Lock()
	v, found := c.items[k]
	if !found || v.Expired() {
		c.mu.Unlock()
		return 0, fmt.Errorf("item %s not found", k)
	}
	rv, ok := any(v.Object).(float64)
	if !ok {
		c.mu.Unlock()
		return 0, fmt.Errorf("the value for %s is not an float64", k)
	}
	nv := rv - n
	v.Object = any(nv).(T)
	c.items[k] = v
	c.mu.Unlock()
	return nv, nil
}

// Delete an item from the cache. Does nothing if the key is not in the cache.
func (c *cache[T]) Delete(k string) {
	c.mu.Lock()
	v, evicted := c.delete(k)
	c.mu.Unlock()
	if evicted {
		c.onEvicted(k, v)
	}
}

func (c *cache[T]) delete(k string) (interface{}, bool) {
	if c.onEvicted != nil {
		if v, found := c.items[k]; found {
			delete(c.items, k)
			return v.Object, true
		}
	}
	delete(c.items, k)
	return nil, false
}

type keyAndValue struct {
	key   string
	value interface{}
}

// DeleteExpired Delete all expired items from the cache.
func (c *cache[T]) DeleteExpired() {
	var evictedItems []keyAndValue
	now := time.Now().UnixNano()
	c.mu.Lock()
	for k, v := range c.items {
		// "Inlining" of expired
		if v.Expiration > 0 && now > v.Expiration {
			ov, evicted := c.delete(k)
			if evicted {
				evictedItems = append(evictedItems, keyAndValue{k, ov})
			}
		}
	}
	c.mu.Unlock()
	for _, v := range evictedItems {
		c.onEvicted(v.key, v.value)
	}
}

// OnEvicted Sets an (optional) function that is called with the key and value when an
// item is evicted from the cache. (Including when it is deleted manually, but
// not when it is overwritten.) Set to nil to disable.
func (c *cache[T]) OnEvicted(f func(string, interface{})) {
	c.mu.Lock()
	c.onEvicted = f
	c.mu.Unlock()
}

// Save Write the cache's items (using Gob) to an io.Writer.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache[T]) Save(w io.Writer) (err error) {
	enc := gob.NewEncoder(w)
	defer func() {
		if x := recover(); x != nil {
			err = fmt.Errorf("error registering item types with Gob library")
		}
	}()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, v := range c.items {
		gob.Register(v.Object)
	}
	err = enc.Encode(&c.items)
	return
}

// SaveFile Save the cache's items to the given filename, creating the file if it
// doesn't exist, and overwriting it if it does.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache[T]) SaveFile(fname string) error {
	fp, err := os.Create(fname)
	if err != nil {
		return err
	}
	err = c.Save(fp)
	if err != nil {
		_ = fp.Close()
		return err
	}
	return fp.Close()
}

// Load Add (Gob-serialized) cache items from an io.Reader, excluding any items with
// keys that already exist (and haven't expired) in the current cache.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache[T]) Load(r io.Reader) error {
	dec := gob.NewDecoder(r)
	items := map[string]Item[T]{}
	err := dec.Decode(&items)
	if err == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		for k, v := range items {
			ov, found := c.items[k]
			if !found || ov.Expired() {
				c.items[k] = v
			}
		}
	}
	return err
}

// LoadFile Load and add cache items from the given filename, excluding any items with
// keys that already exist in the current cache.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache[T]) LoadFile(filename string) error {
	fp, err := os.Open(filename)
	if err != nil {
		return err
	}
	err = c.Load(fp)
	if err != nil {
		_ = fp.Close()
		return err
	}
	return fp.Close()
}

// Items Copies all unexpired items in the cache into a new map and returns it.
func (c *cache[T]) Items() map[string]Item[T] {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m := make(map[string]Item[T], len(c.items))
	now := time.Now().UnixNano()
	for k, v := range c.items {
		// "Inlining" of Expired
		if v.Expiration > 0 {
			if now > v.Expiration {
				continue
			}
		}
		m[k] = v
	}
	return m
}

// ItemCount Returns the number of items in the cache. This may include items that have
// expired, but have not yet been cleaned up.
func (c *cache[T]) ItemCount() int {
	c.mu.RLock()
	n := len(c.items)
	c.mu.RUnlock()
	return n
}

// Flush Delete all items from the cache.
func (c *cache[T]) Flush() {
	c.mu.Lock()
	c.items = map[string]Item[T]{}
	c.mu.Unlock()
}

type janitor[T any] struct {
	Interval time.Duration
	stop     chan bool
}

func (j *janitor[T]) Run(c *cache[T]) {
	ticker := time.NewTicker(j.Interval)
	for {
		select {
		case <-ticker.C:
			c.DeleteExpired()
		case <-j.stop:
			ticker.Stop()
			return
		}
	}
}

func stopJanitor[T any](c *Cache[T]) {
	c.janitor.stop <- true
}

func runJanitor[T any](c *cache[T], ci time.Duration) {
	j := &janitor[T]{
		Interval: ci,
		stop:     make(chan bool),
	}
	c.janitor = j
	go j.Run(c)
}

func newCache[T any](de time.Duration, m map[string]Item[T]) *cache[T] {
	if de == 0 {
		de = -1
	}
	c := &cache[T]{
		defaultExpiration: de,
		items:             m,
	}
	return c
}

func newCacheWithJanitor[T any](de time.Duration, ci time.Duration, m map[string]Item[T]) *Cache[T] {
	c := newCache(de, m)
	// This trick ensures that the janitor goroutine (which--granted it
	// was enabled--is running DeleteExpired on c forever) does not keep
	// the returned C object from being garbage collected. When it is
	// garbage collected, the finalizer stops the janitor goroutine, after
	// which c can be collected.
	C := &Cache[T]{c}
	if ci > 0 {
		runJanitor(c, ci)
		runtime.SetFinalizer(C, stopJanitor[T])
	}
	return C
}

func WithDefaultExpiration(defaultExpiration time.Duration) Option {
	return func(c *config) {
		c.defaultExpiration = defaultExpiration
	}
}

func WithCleanupInterval(cleanupInterval time.Duration) Option {
	return func(c *config) {
		c.cleanupInterval = cleanupInterval
	}
}

func WithItems[T any](items map[string]Item[T]) Option {
	return func(c *config) {
		c.items = map[string]any{}
		for k, v := range items {
			c.items[k] = v
		}
	}
}

type Option func(c *config)

type config struct {
	defaultExpiration time.Duration
	cleanupInterval   time.Duration
	items             map[string]any
}

// New Return a new cache with a given default expiration duration and cleanup
// interval. If the expiration duration is less than one (or NoExpiration),
// the items in the cache never expire (by default), and must be deleted
// manually. If the cleanup interval is less than one, expired items are not
// deleted from the cache before calling c.DeleteExpired().
func New[T any](options ...Option) *Cache[T] {
	c := &config{
		cleanupInterval: 5 * time.Second,
	}
	for _, v := range options {
		v(c)
	}
	items := make(map[string]Item[T])
	for k, v := range c.items {
		items[k] = v.(Item[T])
	}
	return newCacheWithJanitor(c.defaultExpiration, c.cleanupInterval, items)
}
