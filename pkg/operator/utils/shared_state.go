package utils

import "sync"

type OperatorSharedState struct {
	csiConfigMapCreated bool
	sync.RWMutex
}

var globalSharedState *OperatorSharedState

var initStateSync sync.Once

func InitGlobalState() {
	initStateSync.Do(func() {
		globalSharedState = &OperatorSharedState{}
	})
}

func GetGlobalSharedState() *OperatorSharedState {
	return globalSharedState
}

func (g *OperatorSharedState) GetCSIConfigState() bool {
	g.RLock()
	defer g.RUnlock()
	return g.csiConfigMapCreated
}

func (g *OperatorSharedState) SetCSIConfigState(state bool) {
	g.Lock()
	defer g.Unlock()
	g.csiConfigMapCreated = state
}
