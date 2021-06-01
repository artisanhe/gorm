package gorm_test

import (
	"testing"
	"time"
)

func TestBatchCreate(t *testing.T) {
	float := 35.03554004971999
	user := User{Name: "CreateUser", Age: 18, Birthday: time.Now(), UserNum: Num(111), PasswordHash: []byte{'f', 'a', 'k', '4'}, Latitude: float}

	users := []User{user, user, user}
	if err := DB.BatchCreate(users); err != nil {
		t.Error("batch create shoud be success")
	}
}
