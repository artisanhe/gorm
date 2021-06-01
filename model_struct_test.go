package gorm

import (
	"github.com/stretchr/testify/assert"
	"reflect"
	"testing"
)

func TestParseTagSetting(t *testing.T) {
	tt := assert.New(t)

	tagSettings := ParseTagSetting(reflect.StructTag(`gorm:"column:F_enabled" sql:"type:tinyint(8) unsigned;not null;default:1;unique_index:I_organization;unique_index:I_certificate"`))
	tt.Equal("I_organization:I_certificate", tagSettings["UNIQUE_INDEX"])
}
