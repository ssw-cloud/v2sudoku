package runtime

import panel "github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/panel"

type Server interface {
	Start() error
	Close() error
	SetAliveList(alive map[int]int)
	UpdateUsers(added, deleted, modified, full []panel.UserInfo) error
	GetUserTrafficSlice(reportMin int) []panel.UserTraffic
	ConfirmUserTraffic(reported []panel.UserTraffic)
	GetOnlineDevice() []panel.OnlineUser
}
