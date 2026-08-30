package api

import "sync"

// flightGroup coalesces identical in-flight work so N callers sharing a
// key run fn once. Unlike cache, this only covers the overlap window.
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

func (g *flightGroup) Do(key string, fn func() (interface{}, error)) (v interface{}, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*flightCall)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := &flightCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	c.wg.Done()
	return c.val, c.err, false
}
