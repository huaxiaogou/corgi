package main

import "github.com/kiperjing/corgi/internal/store"

func openStore(path string) (*store.Store, error) {
	return store.Open(path)
}
