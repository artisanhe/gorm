package gorm

import (
	"database/sql"
	"fmt"
	"go/ast"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type safeModelStructsMap struct {
	m map[reflect.Type]*ModelStruct
	l *sync.RWMutex
}

func (s *safeModelStructsMap) Set(key reflect.Type, value *ModelStruct) {
	s.l.Lock()
	defer s.l.Unlock()
	s.m[key] = value
}

func (s *safeModelStructsMap) Get(key reflect.Type) *ModelStruct {
	s.l.RLock()
	defer s.l.RUnlock()
	return s.m[key]
}

func newModelStructsMap() *safeModelStructsMap {
	return &safeModelStructsMap{l: new(sync.RWMutex), m: make(map[reflect.Type]*ModelStruct)}
}

//var modelStructs = map[reflect.Type]*ModelStruct{}
var modelStructs = newModelStructsMap()

var DefaultTableNameHandler = func(db *DB, defaultTableName string) string {
	return defaultTableName
}

type ModelStruct struct {
	PrimaryFields    []*StructField
	StructFields     []*StructField
	ModelType        reflect.Type
	defaultTableName string
}

func (s ModelStruct) TableName(db *DB) string {
	return DefaultTableNameHandler(db, s.defaultTableName)
}

type StructField struct {
	DBName          string
	Name            string
	Names           []string
	IsPrimaryKey    bool
	IsNormal        bool
	IsIgnored       bool
	IsScanner       bool
	HasDefaultValue bool
	Tag             reflect.StructTag
	Struct          reflect.StructField
	IsForeignKey    bool
	IsAutoIncrement bool
	Relationship    *Relationship
}

func (structField *StructField) clone() *StructField {
	return &StructField{
		DBName:          structField.DBName,
		Name:            structField.Name,
		Names:           structField.Names,
		IsPrimaryKey:    structField.IsPrimaryKey,
		IsNormal:        structField.IsNormal,
		IsIgnored:       structField.IsIgnored,
		IsScanner:       structField.IsScanner,
		HasDefaultValue: structField.HasDefaultValue,
		Tag:             structField.Tag,
		Struct:          structField.Struct,
		IsForeignKey:    structField.IsForeignKey,
		Relationship:    structField.Relationship,
		IsAutoIncrement: structField.IsAutoIncrement,
	}
}

type Relationship struct {
	Kind                        string
	PolymorphicType             string
	PolymorphicDBName           string
	ForeignFieldName            string
	ForeignDBName               string
	AssociationForeignFieldName string
	AssociationForeignDBName    string
	JoinTableHandler            JoinTableHandlerInterface
}

var pluralMapKeys = []*regexp.Regexp{regexp.MustCompile("ch$"), regexp.MustCompile("ss$"), regexp.MustCompile("sh$"), regexp.MustCompile("day$"), regexp.MustCompile("y$"), regexp.MustCompile("x$"), regexp.MustCompile("([^s])s?$")}
var pluralMapValues = []string{"ches", "sses", "shes", "days", "ies", "xes", "${1}s"}

func (scope *Scope) GetModelStruct() *ModelStruct {
	var modelStruct ModelStruct

	reflectValue := reflect.Indirect(reflect.ValueOf(scope.Value))

	if !reflectValue.IsValid() {
		return &modelStruct
	}

	if reflectValue.Kind() == reflect.Slice {
		reflectValue = reflect.Indirect(reflect.New(reflectValue.Type().Elem()))
	}

	scopeType := reflectValue.Type()

	if scopeType.Kind() == reflect.Ptr {
		scopeType = scopeType.Elem()
	}

	//if value, ok := modelStructs[scopeType]; ok {
	//return value
	//}
	if value := modelStructs.Get(scopeType); value != nil {
		return value
	}

	modelStruct.ModelType = scopeType
	if scopeType.Kind() != reflect.Struct {
		return &modelStruct
	}

	// Set tablename
	type tabler interface {
		TableName() string
	}

	if tb, ok := reflect.ValueOf(scope.Value).Interface().(tabler); ok {
		modelStruct.defaultTableName = tb.TableName()
	} else if tb, ok := reflect.Indirect(reflect.ValueOf(scope.Value)).Interface().(tabler); ok {
		modelStruct.defaultTableName = tb.TableName()
	} else if tb, ok := reflect.New(scopeType).Interface().(tabler); ok {
		modelStruct.defaultTableName = tb.TableName()
	} else {
		name := ToDBName(scopeType.Name())
		if scope.db == nil || !scope.db.parent.singularTable {
			for index, reg := range pluralMapKeys {
				if reg.MatchString(name) {
					name = reg.ReplaceAllString(name, pluralMapValues[index])
				}
			}
		}

		modelStruct.defaultTableName = name
	}

	// Get all fields
	fields := []*StructField{}
	for i := 0; i < scopeType.NumField(); i++ {
		if fieldStruct := scopeType.Field(i); ast.IsExported(fieldStruct.Name) {
			field := &StructField{
				Struct: fieldStruct,
				Name:   fieldStruct.Name,
				Names:  []string{fieldStruct.Name},
				Tag:    fieldStruct.Tag,
			}

			if fieldStruct.Tag.Get("sql") == "-" {
				field.IsIgnored = true
			} else {
				gormSettings := ParseTagSetting(field.Tag)
				if _, ok := gormSettings["PRIMARY_KEY"]; ok {
					field.IsPrimaryKey = true
					modelStruct.PrimaryFields = append(modelStruct.PrimaryFields, field)
				}

				if _, ok := gormSettings["DEFAULT"]; ok {
					field.HasDefaultValue = true
				}

				if _, ok := gormSettings["AUTO_INCREMENT"]; ok {
					field.IsAutoIncrement = true
				}

				if value, ok := gormSettings["COLUMN"]; ok {
					field.DBName = value
				} else {
					field.DBName = ToDBName(fieldStruct.Name)
				}
			}
			fields = append(fields, field)
		}
	}

	defer func() {
		for _, field := range fields {
			if !field.IsIgnored {
				fieldStruct := field.Struct
				fieldType, indirectType := fieldStruct.Type, fieldStruct.Type
				if indirectType.Kind() == reflect.Ptr {
					indirectType = indirectType.Elem()
				}

				if _, isScanner := reflect.New(fieldType).Interface().(sql.Scanner); isScanner {
					field.IsScanner, field.IsNormal = true, true
				}

				if _, isTime := reflect.New(indirectType).Interface().(*time.Time); isTime {
					field.IsNormal = true
				}

				if !field.IsNormal {
					gormSettings := ParseTagSetting(field.Tag)
					toScope := scope.New(reflect.New(fieldStruct.Type).Interface())

					getForeignField := func(column string, fields []*StructField) *StructField {
						for _, field := range fields {
							if field.Name == column || field.DBName == ToDBName(column) {
								return field
							}
						}
						return nil
					}

					var relationship = &Relationship{}

					foreignKey := ""
					if _, ok := gormSettings["FOREIGNKEY"]; ok {
						foreignKey = gormSettings["FOREIGNKEY"]
					}
					if _, ok := gormSettings["POLYMORPHIC"]; ok {
						polymorphic := gormSettings["POLYMORPHIC"]
						if polymorphicField := getForeignField(polymorphic+"Id", toScope.GetStructFields()); polymorphicField != nil {
							if polymorphicType := getForeignField(polymorphic+"Type", toScope.GetStructFields()); polymorphicType != nil {
								relationship.ForeignFieldName = polymorphicField.Name
								relationship.ForeignDBName = polymorphicField.DBName
								relationship.PolymorphicType = polymorphicType.Name
								relationship.PolymorphicDBName = polymorphicType.DBName
								polymorphicType.IsForeignKey = true
								polymorphicField.IsForeignKey = true
							}
						}
					}

					switch indirectType.Kind() {
					case reflect.Slice:
						elemType := indirectType.Elem()
						if elemType.Kind() == reflect.Ptr {
							elemType = elemType.Elem()
						}

						if elemType.Kind() == reflect.Struct {
							if foreignKey == "" {
								foreignKey = scopeType.Name() + "Id"
							}

							if _, ok := gormSettings["MANY2MANY"]; ok {
								many2many := gormSettings["MANY2MANY"]
								relationship.Kind = "many_to_many"
								associationForeignKey := ""
								if _, ok := gormSettings["ASSOCIATIONFOREIGNKEY"]; ok {
									associationForeignKey = gormSettings["ASSOCIATIONFOREIGNKEY"]
								}
								if associationForeignKey == "" {
									associationForeignKey = elemType.Name() + "Id"
								}

								relationship.ForeignFieldName = foreignKey
								relationship.ForeignDBName = ToDBName(foreignKey)
								relationship.AssociationForeignFieldName = associationForeignKey
								relationship.AssociationForeignDBName = ToDBName(associationForeignKey)

								joinTableHandler := JoinTableHandler{}
								joinTableHandler.Setup(relationship, many2many, scopeType, elemType)
								relationship.JoinTableHandler = &joinTableHandler
								field.Relationship = relationship
							} else {
								relationship.Kind = "has_many"
								if foreignField := getForeignField(foreignKey, toScope.GetStructFields()); foreignField != nil {
									relationship.ForeignFieldName = foreignField.Name
									relationship.ForeignDBName = foreignField.DBName
									foreignField.IsForeignKey = true
									field.Relationship = relationship
								} else if relationship.ForeignFieldName != "" {
									field.Relationship = relationship
								}
							}
						} else {
							field.IsNormal = true
						}
					case reflect.Struct:
						if _, ok := gormSettings["EMBEDDED"]; ok || fieldStruct.Anonymous {
							for _, toField := range toScope.GetStructFields() {
								toField = toField.clone()
								toField.Names = append([]string{fieldStruct.Name}, toField.Names...)
								modelStruct.StructFields = append(modelStruct.StructFields, toField)
								if toField.IsPrimaryKey {
									modelStruct.PrimaryFields = append(modelStruct.PrimaryFields, toField)
								}
							}
							continue
						} else {
							belongsToForeignKey := foreignKey
							if belongsToForeignKey == "" {
								belongsToForeignKey = field.Name + "Id"
							}

							if foreignField := getForeignField(belongsToForeignKey, fields); foreignField != nil {
								relationship.Kind = "belongs_to"
								relationship.ForeignFieldName = foreignField.Name
								relationship.ForeignDBName = foreignField.DBName
								foreignField.IsForeignKey = true
								field.Relationship = relationship
							} else {
								if foreignKey == "" {
									foreignKey = modelStruct.ModelType.Name() + "Id"
								}
								relationship.Kind = "has_one"
								if foreignField := getForeignField(foreignKey, toScope.GetStructFields()); foreignField != nil {
									relationship.ForeignFieldName = foreignField.Name
									relationship.ForeignDBName = foreignField.DBName
									foreignField.IsForeignKey = true
									field.Relationship = relationship
								} else if relationship.ForeignFieldName != "" {
									field.Relationship = relationship
								}
							}
						}
					default:
						field.IsNormal = true
					}
				}

				if field.IsNormal {
					if len(modelStruct.PrimaryFields) == 0 && field.DBName == "id" {
						field.IsPrimaryKey = true
						modelStruct.PrimaryFields = append(modelStruct.PrimaryFields, field)
					}
				}
			}
			modelStruct.StructFields = append(modelStruct.StructFields, field)
		}
	}()

	//modelStructs[scopeType] = &modelStruct
	modelStructs.Set(scopeType, &modelStruct)

	return &modelStruct
}

func (scope *Scope) GetStructFields() (fields []*StructField) {
	return scope.GetModelStruct().StructFields
}

func (scope *Scope) generateSqlTag(field *StructField) string {
	var sqlType string
	structType := field.Struct.Type
	if structType.Kind() == reflect.Ptr {
		structType = structType.Elem()
	}
	reflectValue := reflect.Indirect(reflect.New(structType))
	sqlSettings := ParseTagSetting(field.Tag)

	if value, ok := sqlSettings["TYPE"]; ok {
		sqlType = value
	}

	additionalType := ""
	if value, ok := sqlSettings["NOT NULL"]; ok {
		additionalType = additionalType + value
	}
	additionalType = additionalType + " "
	if value, ok := sqlSettings["UNIQUE"]; ok {
		additionalType = additionalType + value
	}
	if value, ok := sqlSettings["DEFAULT"]; ok {
		additionalType = additionalType + " DEFAULT " + value
	}

	if field.IsScanner {
		var getScannerValue func(reflect.Value)
		getScannerValue = func(value reflect.Value) {
			reflectValue = value
			if _, isScanner := reflect.New(reflectValue.Type()).Interface().(sql.Scanner); isScanner && reflectValue.Kind() == reflect.Struct {
				getScannerValue(reflectValue.Field(0))
			}
		}
		getScannerValue(reflectValue)
	}

	if sqlType == "" {
		var size = 255

		if value, ok := sqlSettings["SIZE"]; ok {
			size, _ = strconv.Atoi(value)
		}

		_, autoIncrease := sqlSettings["AUTO_INCREMENT"]
		if field.IsPrimaryKey {
			autoIncrease = true
		}

		sqlType = scope.Dialect().SqlTag(reflectValue, size, autoIncrease)
		if field.Tag.Get("sql") != "" {
			fmt.Println(fmt.Sprintf("[warning]field[%s] sql tag has no type", field.Name))
		}
	}

	if strings.TrimSpace(additionalType) == "" {
		return sqlType
	} else {
		return fmt.Sprintf("%v %v", sqlType, additionalType)
	}
}

func (scope *Scope) compareFieldAndColumn(field *StructField, column string) bool {
	gormMap := ParseTagSetting(field.Tag)
	if _, ok := gormMap["IGNORE_MIGRATE"]; ok {
		return true
	}
	sqlTag := scope.generateSqlTag(field)
	tagParts := strings.Split(sqlTag, " ")
	columnParts := strings.Split(column, " ")
	i := 0
	j := 0
	for i < len(tagParts) && j < len(columnParts) {
		if tagParts[i] == "" {
			i++
			continue
		}

		if columnParts[j] == "" {
			j++
			continue
		}

		if strings.ToUpper(tagParts[i]) != strings.ToUpper(columnParts[j]) {
			return false
		}
		i++
		j++
	}
	return true
}

func ParseTagSetting(tags reflect.StructTag) map[string]string {
	setting := map[string]string{}
	for _, str := range []string{tags.Get("sql"), tags.Get("gorm")} {
		tags := strings.Split(str, ";")
		for _, value := range tags {
			v := strings.Split(value, ":")
			k := strings.TrimSpace(strings.ToUpper(v[0]))
			if len(v) >= 2 {
				setVal(setting, k, strings.Join(v[1:], ":"))
			} else {
				setVal(setting, k, k)
			}
		}
	}
	return setting
}

func setVal(s map[string]string, key string, value string) {
	if v, exists := s[key]; exists {
		s[key] = strings.Join([]string{v, value}, ":")
	} else {
		s[key] = value
	}
}
