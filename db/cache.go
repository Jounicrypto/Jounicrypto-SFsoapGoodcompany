package db

import (
	"reflect"
	"sync"
)

// CacheEnabler makes it possible to mark a type cacheable, the Get and Set methods can be auto-generated
// see dbinit.go and internal/dbgen/main.go as well as the structs.go of your main project
type CacheEnabler interface {
	MaxNumItemsCache() int
}

// Cacher is the actual implementation of the Caching for individual items
type Cacher interface {
	Get(int64) interface{}
	Set(int64, interface{})
}

// AllCacher can be used when all tuples of a type need to be kept / should be kept in memory. This can be useful
// for types such as Provinces and Countries. The argument is usually an order by clause such as
// ORDER BY Id DESC or something of the sort.
type AllCacher interface {
	SetAll(string, []interface{})
	GetAll(string) ([]interface{}, bool)
}

type ItemCacher struct {
	cache    map[int64]interface{}
	lock     sync.RWMutex
	allOrder map[string][]int64
	MaxItems int
}

func (i ItemCacher) GetAll(orderByClause string) ([]interface{}, bool) {
	i.lock.RLock()
	defer func() { i.lock.RUnlock() }()
	if ids, ok := i.allOrder[orderByClause]; ok {
		return i.multiGetArray(ids), ok
	}
	return nil, false
}

func (i ItemCacher) SetAll(orderByClause string, ds []int64) {
	res := make([]int64, 0)
	for _, d := range ds {
		if d != 0 {
			res = append(res, d)
		}
	}
	i.lock.Lock()
	defer func() { i.lock.Unlock() }()
	i.allOrder[orderByClause] = res
}

func (i ItemCacher) Get(id int64) interface{} {
	i.lock.RLock()
	defer func() { i.lock.RUnlock() }()
	res := i.cache[id]
	return res
}

func (i ItemCacher) Set(id int64, d interface{}) {
	i.lock.Lock()
	defer func() { i.lock.Unlock() }()
	i.set(id, d)
}

func (i ItemCacher) set(id int64, d interface{}) {
	if reflect.TypeOf(d).Kind() == reflect.Ptr {
		d = reflect.Indirect(reflect.ValueOf(d)).Interface()
	}
	if i.MaxItems == 0 {
		i.cache[id] = d
		i.clearOrderCache()
	} else if len(i.cache) < i.MaxItems {
		i.cache[id] = d
		i.clearOrderCache()
	} else {
		// Brute force - should do something less stupid here eventually
		i.clear()
		i.cache[id] = d
	}
}

func (i ItemCacher) MultiGet(ids []int64) map[int64]interface{} {
	res := make(map[int64]interface{})
	i.lock.Lock()
	defer func() { i.lock.Unlock() }()
	for _, id := range ids {
		if item, ok := i.cache[id]; ok {
			res[id] = item
		}
	}
	return res
}

func (i ItemCacher) multiGetArray(ids []int64) []interface{} {
	res := []interface{}{}
	for _, id := range ids {
		if item, ok := i.cache[id]; ok {
			res = append(res, item)
		}
	}
	return res
}

func (i ItemCacher) MultiGetFunc(ids []int64, addToMap func([]int64, *map[int64]interface{}) error) (map[int64]interface{}, error) {
	res := make(map[int64]interface{})
	var missed []int64
	i.lock.Lock()
	for _, id := range ids {
		if item, ok := i.cache[id]; ok {
			res[id] = item
		} else {
			missed = append(missed, id)
		}
	}
	i.lock.Unlock()
	if len(missed) > 0 {
		if err := addToMap(missed, &res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (i ItemCacher) MultiSet(ds map[int64]interface{}) {
	i.lock.Lock()
	defer func() { i.lock.Unlock() }()
	for k, v := range ds {
		if reflect.TypeOf(v).Kind() == reflect.Ptr {
			v = reflect.Indirect(reflect.ValueOf(v)).Interface()
		}
		if i.MaxItems == 0 {
			i.cache[k] = v
			i.clearOrderCache()
		} else if len(i.cache) < i.MaxItems {
			i.cache[k] = v
			i.clearOrderCache()
		} else {
			// Brute force - should do something less stupid here eventually
			i.clear()
			i.cache[k] = v
		}
	}
}

func (i ItemCacher) Remove(id int64) interface{} {
	i.lock.Lock()
	defer func() { i.lock.Unlock() }()
	res := i.cache[id]
	delete(i.cache, id)
	i.clearOrderCache()
	return res
}

func (i ItemCacher) Clear() {
	i.lock.Lock()
	defer func() { i.lock.Unlock() }()
	i.clear()
}

func (i ItemCacher) clear() {
	i.cache = make(map[int64]interface{})
	i.clearOrderCache()
}

func (i ItemCacher) clearOrderCache() {
	i.allOrder = make(map[string][]int64)
}

func (i ItemCacher) ClearOrderCache() {
	i.allOrder = make(map[string][]int64)
}
