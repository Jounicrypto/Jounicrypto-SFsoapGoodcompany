package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type Crud struct {
	Insert, Update, Delete, SelectByPk string
	ColNames, RawColNames              []string
	Cacher                             ItemCacher
}

var CrudStatements = make(map[string]Crud)

type loginDb struct {
	driverName, dataSourceName string
}

type Validation struct {
	Errs  map[string]string
	Warns map[string]string
	Infos map[string]string
}

var datasource loginDb

func InitDb(driverName, dataSourceName string) {
	datasource.driverName = driverName
	datasource.dataSourceName = dataSourceName
}

func InitCrud(i interface{}) error {
	if i == nil {
		return errors.New("nil interface{} in InitCrud")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	name := e.Name()
	if _, ok := CrudStatements[name]; ok {
		return errors.New("already initialized " + name + " in InitCrud")
	}

	c := Crud{
		Insert:      genInsertStmt(e),
		Update:      genUpdateStmt(e),
		Delete:      genDeleteStmt(e),
		SelectByPk:  genSelectByIdStmt(e),
		ColNames:    colNames(e),
		RawColNames: rawColNames(e),
		Cacher: ItemCacher{
			cache:    make(map[int64]interface{}),
			lock:     sync.RWMutex{},
			allOrder: make(map[string][]int64),
			MaxItems: 0,
		},
	}
	CrudStatements[name] = c
	return nil
}

func GetCrud(i interface{}) *Crud {
	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	name := e.Name()

	if c, ok := CrudStatements[name]; ok {
		return &c
	}
	return nil
}

func CreateTableAndInitCrud(conn *sql.DB, i interface{}, createTable bool, dropFirst bool) error {
	if err := InitCrud(i); err != nil {
		return err
	}

	if dropFirst {
		if err := dropTable(conn, i); err != nil {
			log.Println("could not remove table ", reflect.TypeOf(i).Name(), " ", err)
		}
	}

	if createTable {
		stmts := genTable(conn, i)
		if len(stmts) > 0 {
			if err := executeStmts(conn, stmts); err != nil {
				log.Fatal("Could not create table ", err)
			}
			if s, ok := i.(Seeder); ok && createTable {
				if crud := GetCrud(i); crud != nil {
					if err := s.Seed(conn, crud); err != nil {
						log.Fatal("could not seed table ", reflect.TypeOf(i).Name(), " err: ", err)
					}
				} else {
					log.Fatal("somehow there is no crud for i ", reflect.TypeOf(i).Name())
				}
			}
		}
	}

	return nil
}

func executeStmts(db *sql.DB, stmts []string) error {
	for _, stmt := range stmts {
		rows, err := db.Query(stmt)
		if err != nil {
			return errors.New("Could not execute stmt " + stmt + " " + err.Error())
		}
		_ = rows.Close()
	}
	return nil
}

func Update(conn *sql.DB, i interface{}) (int64, error) {
	if i == nil {
		return 0, errors.New("nil interface{} in Update")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	name := e.Name()
	var c Crud
	var ok bool
	if c, ok = CrudStatements[name]; !ok {
		return 0, errors.New("not initialized " + name + " in Update")
	}

	if v, ok := i.(Validator); ok {
		vl := Validation{}
		if err := v.PreUpdate(i, &vl); err != nil {
			return 0, err
		}
	}
	var args []interface{}
	for _, name := range c.RawColNames {
		if name == "Id" {
			continue
		}
		args = append(args, reflect.ValueOf(i).FieldByName(name).Interface())
	}
	id := reflect.ValueOf(i).FieldByName("Id").Int()
	args = append(args, id)

	res, err := conn.Exec(c.Update, args...)
	if err != nil {
		return 0, errors.New(fmt.Sprintf("could not execute %s ; %v", c.Update, err))
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return 0, errors.New(fmt.Sprintf("could not fetch num affected rows for %s ; %v", c.Update, err))
	}
	c.Cacher.Set(id, i)

	if h, ok := i.(PostHooker); ok {
		h.PostUpdate(i)
	}
	return rowsAffected, nil
}

func Delete(conn *sql.DB, i interface{}) (int64, error) {
	if i == nil {
		return 0, errors.New("nil interface{} in Delete")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	name := e.Name()
	var c Crud
	var ok bool
	if c, ok = CrudStatements[name]; !ok {
		return 0, errors.New("not initialized " + name + " in Delete")
	}

	var args []interface{}
	id := reflect.ValueOf(i).FieldByName("Id").Int()
	args = append(args, id)

	res, err := conn.Exec(c.Delete, args...)
	if err != nil {
		return 0, errors.New(fmt.Sprintf("could not execute %s ; %v", c.Delete, err))
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return 0, errors.New(fmt.Sprintf("could not delete with %s, could not fetch rows affected ; %v", c.Delete, err))
	}
	c.Cacher.Remove(id)
	return rowsAffected, nil
}

func Insert(conn *sql.DB, i interface{}) (int64, error) {
	if i == nil {
		return 0, errors.New("nil interface{} in Insert")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	name := e.Name()
	var c Crud
	var ok bool
	if c, ok = CrudStatements[name]; !ok {
		return 0, errors.New("not initialized " + name + " in Insert")
	}

	if v, ok := i.(Validator); ok {
		vl := Validation{}
		if err := v.PreCreate(i, &vl); err != nil {
			return 0, err
		}
	}
	var args []interface{}
	for _, name := range c.RawColNames {
		if name == "Id" {
			continue
		}
		// else if name == "Updated" || name == "Created" {
		// 	continue
		// }
		f, _ := reflect.TypeOf(i).FieldByName(name)
		vi := reflect.ValueOf(i).FieldByName(name).Interface()
		if f.Type.String() == "time.Time" {
			minT := time.Date(1001, 1, 1, 0, 0, 1, 0, time.UTC)
			if v, ok := vi.(time.Time); ok && minT.Before(v) {
				args = append(args, v)
			} else {
				args = append(args, minT)
			}
		} else {
			args = append(args, vi)
		}
	}

	res, err := conn.Exec(c.Insert, args...)

	if err != nil {
		return 0, errors.New(fmt.Sprintf("could not execute %s ; %v", c.Insert, err))
	}

	lid, err := res.LastInsertId()
	if err != nil {
		return 0, errors.New(fmt.Sprintf("could not fetch last insert id for %s ; %v", c.Insert, err))
	}

	if reflect.TypeOf(i).Kind() != reflect.Ptr {
		i = ToStructPtr(i)
	}
	reflect.ValueOf(i).Elem().FieldByName("Id").SetInt(lid)

	i, err = SelectById(conn, reflect.ValueOf(i).Elem().Interface())
	if err != nil {
		return 0, errors.New(fmt.Sprintf("could not select by id for %s ; %v", c.Insert, err))
	}
	c.Cacher.Set(lid, i)
	if h, ok := i.(PostHooker); ok {
		h.PostCreate(i)
	}

	return lid, nil
}
func ToStructPtr(obj interface{}) interface{} {
	val := reflect.ValueOf(obj)
	vp := reflect.New(val.Type())
	vp.Elem().Set(val)
	return vp.Interface()
}
func GetDB() *sql.DB {
	if datasource.driverName == "" {
		log.Fatal("no drivername supplied, call InitDb first")
	}
	db, err := sql.Open(datasource.driverName, datasource.dataSourceName)
	if err != nil {
		log.Fatal("Could not obtain db connection", err)
	} else if db == nil {
		log.Fatal("Could not obtain db connection ; db == nil")
	}
	if err = db.Ping(); err != nil {
		log.Fatal("Could not ping db", err)
	}
	return db
}

func SelectCountOfType(db *sql.DB, i interface{}) (int64, error) {
	if i == nil {
		return 0, errors.New("nil interface in SelectCountOfType")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	sqlStmt := "SELECT COUNT(1) FROM `" + e.Name() + "`"
	stmt, err := db.Prepare(sqlStmt)
	if err != nil {
		return 0, errors.New("statement (prepare) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = stmt.Close() }()
	rows, err := stmt.Query()
	if err != nil {
		return 0, errors.New("statement (query) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		var count int64
		if err = rows.Scan(&count); err != nil {
			return 0, errors.New("problem scanning results sql: " + sqlStmt + " err: " + err.Error())
		}
		return count, nil
	}
	return 0, nil
}

func SelectCountByStruct(db *sql.DB, i interface{}) (int64, error) {
	if i == nil {
		return 0, errors.New("nil interface in SelectCountByStruct")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	val := reflect.ValueOf(i)
	var whereParts []string
	var valParts []interface{}
	for walk, maxLen := 0, e.NumField(); walk < maxLen; walk++ {
		f := e.Field(walk)
		s := val.Field(walk)
		switch f.Type.Name() {
		case "int64":
			num := s.Int()
			if num != 0 {
				whereParts = append(whereParts, fmt.Sprintf("`"+f.Name+"` = %d", num))
			}
		case "string":
			sval := s.String()
			if sval != "" {
				whereParts = append(whereParts, "`"+f.Name+"` = ?")
				valParts = append(valParts, sval)
			}
		}
	}

	var sqlStmt string
	if len(whereParts) > 0 {
		sqlStmt = "SELECT COUNT(1) FROM `" + e.Name() + "` WHERE " + strings.Join(whereParts, " AND ")
	} else {
		sqlStmt = "SELECT COUNT(1) FROM `" + e.Name() + "`"
	}

	stmt, err := db.Prepare(sqlStmt)
	if err != nil {
		return 0, errors.New("statement (prepare) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = stmt.Close() }()
	rows, err := stmt.Query(valParts...)
	if err != nil {
		return 0, errors.New("statement (query) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		var count int64
		if err = rows.Scan(&count); err != nil {
			return 0, errors.New("problem scanning results sql: " + sqlStmt + " err: " + err.Error())
		}
		return count, nil
	}
	return 0, nil
}

func SelectById(conn *sql.DB, i interface{}) (interface{}, error) {
	if i == nil {
		return 0, errors.New("nil interface{}")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	name := e.Name()
	var c Crud
	var ok bool
	if c, ok = CrudStatements[name]; !ok {
		return 0, errors.New("not initialized " + name)
	}

	id := reflect.ValueOf(i).FieldByName("Id").Int()
	var args []interface{}
	args = append(args, id)

	if res := c.Cacher.Get(id); res != nil {
		return res, nil
	}
	colNames := colNames(e)

	sqlStmt := "SELECT " + strings.Join(colNames, ", ") + " FROM `" + e.Name() + "` WHERE Id = ?"

	stmt, err := conn.Prepare(sqlStmt)
	if err != nil {
		return nil, errors.New("statement (prepare ; SelectById) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = stmt.Close() }()
	rows, err := stmt.Query(args...)
	if err != nil {
		return nil, errors.New("statement (query ; SelectById) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = rows.Close() }()

	typ := reflect.TypeOf(i)
	orig := reflect.New(typ)
	el := reflect.Indirect(orig)
	for rows.Next() {
		var values []interface{}

		for idx := range colNames {
			f := el.Field(idx)
			v := f.Addr().Interface()
			values = append(values, v)
		}

		if err = rows.Scan(values...); err != nil {
			return nil, errors.New("problem scanning results sql: " + sqlStmt + " err: " + err.Error())
		}
		res := el.Interface()
		c.Cacher.Set(id, res)
		return el.Interface(), nil
	}

	return nil, err
}

func SelectByIdIn(conn *sql.DB, i interface{}, ids []int64) ([]interface{}, error) {
	results, err := SelectByIdsMap(conn, i, ids)
	if err != nil {
		return nil, err
	}

	res := []interface{}{}
	for _, id := range ids {
		item := results[id]
		res = append(res, item)
	}

	return res, err
}

func SelectByIdsMap(conn *sql.DB, i interface{}, ids []int64) (map[int64]interface{}, error) {
	if i == nil {
		return nil, errors.New("nil interface in SelectByIdsMap")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	name := e.Name()
	var c Crud
	var ok bool
	if c, ok = CrudStatements[name]; !ok {
		return nil, errors.New("not initialized " + name + " in SelectByIdsMap")
	}

	selectByIdIn := func(ids []int64, m *map[int64]interface{}) error {
		lIds := len(ids)
		if lIds == 0 {
			return nil
		}
		colNames := colNames(e)
		var sqlStmt = "SELECT " + strings.Join(colNames, ", ") + " FROM `" + e.Name() + "` WHERE Id IN (" + strings.Repeat("?, ", lIds-1) + "?)"
		stmt, err := conn.Prepare(sqlStmt)
		if err != nil {
			return errors.New("statement (prepare ; SelectByIdsMap) " + sqlStmt + " " + err.Error())
		}
		defer func() { _ = stmt.Close() }()
		var valParts []interface{}
		for _, id := range ids {
			valParts = append(valParts, id)
		}
		rows, err := stmt.Query(valParts...)
		if err != nil {
			return errors.New("statement (query ; SelectByIdsMap) " + sqlStmt + " " + err.Error())
		}
		defer func() { _ = rows.Close() }()

		typ := reflect.TypeOf(i)
		orig := reflect.New(typ)
		el := reflect.Indirect(orig)
		mapping := *m
		for rows.Next() {
			var values []interface{}
			for idx := range colNames {
				f := el.Field(idx)
				v := f.Addr().Interface()
				values = append(values, v)
			}

			if err = rows.Scan(values...); err != nil {
				return errors.New("SelectByIdsMap problem scanning results sql: " + sqlStmt + " err: " + err.Error())
			}
			id := el.FieldByName("Id").Int()
			mapping[id] = el.Interface()
			c.Cacher.Set(id, el.Interface())
		}

		return err
	}

	return c.Cacher.MultiGetFunc(ids, selectByIdIn)
}

func SelectByStruct(conn *sql.DB, i interface{}, off, lim int, orderClause string) ([]interface{}, error) {
	if i == nil {
		return nil, errors.New("nil interface in SelectByStruct")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}
	colNames := colNames(e)

	val := reflect.ValueOf(i)
	var whereParts []string
	var valParts []interface{}
	for walk, maxLen := 0, e.NumField(); walk < maxLen; walk++ {
		f := e.Field(walk)
		s := val.Field(walk)
		switch f.Type.Name() {
		case "int64":
			num := s.Int()
			if num != 0 {
				whereParts = append(whereParts, fmt.Sprintf("`"+f.Name+"` = %d", num))
			}
		case "string":
			sval := s.String()
			if sval != "" {
				whereParts = append(whereParts, "`"+f.Name+"` = ?")
				valParts = append(valParts, sval)
			}
		}
	}

	var sqlStmt string
	if len(whereParts) > 0 {
		sqlStmt = "SELECT " + strings.Join(colNames, ", ") + " FROM `" + e.Name() + "` WHERE " + strings.Join(whereParts, " AND ")
	} else {
		sqlStmt = "SELECT " + strings.Join(colNames, ", ") + " FROM `" + e.Name() + "`"
	}

	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(orderClause)), "order by") {
		sqlStmt = fmt.Sprintf("%s %s", sqlStmt, orderClause)
	} else if orderClause != "" {
		sqlStmt = fmt.Sprintf("%s ORDER BY %s", sqlStmt, orderClause)
	}
	if lim > 0 {
		if off < 0 {
			off = 0
		} else if off > 0 {
			sqlStmt = fmt.Sprintf("%s LIMIT %d,%d", sqlStmt, off, lim)
		} else {
			sqlStmt = fmt.Sprintf("%s LIMIT %d", sqlStmt, lim)
		}
	}
	stmt, err := conn.Prepare(sqlStmt)
	if err != nil {
		return nil, errors.New("statement (prepare ; SelectByStruct) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = stmt.Close() }()
	rows, err := stmt.Query(valParts...)
	if err != nil {
		return nil, errors.New("statement (query ; SelectByStruct) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = rows.Close() }()

	var results []interface{}
	typ := reflect.TypeOf(i)
	orig := reflect.New(typ)
	el := reflect.Indirect(orig)
	for rows.Next() {
		var values []interface{}

		for idx := range colNames {
			f := el.Field(idx)
			v := f.Addr().Interface()
			values = append(values, v)
		}

		if err = rows.Scan(values...); err != nil {
			return nil, errors.New("problem scanning results sql: " + sqlStmt + " err: " + err.Error())
		}
		results = append(results, el.Interface())
	}

	return results, err
}

func SelectAll(conn *sql.DB, i interface{}) ([]interface{}, error) {
	return SelectAllOrdered(conn, i, "ORDER BY Id DESC")
}

func SelectAllOrdered(conn *sql.DB, i interface{}, orderClause string) ([]interface{}, error) {
	if i == nil {
		return nil, errors.New("nil interface in SelectAll")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}
	name := e.Name()
	var c Crud
	var ok bool
	if c, ok = CrudStatements[name]; !ok {
		return nil, errors.New("not initialized " + name + " in SelectAll")
	}
	ord := strings.ToLower(strings.TrimSpace(orderClause))
	if all, ok := c.Cacher.GetAll(ord); ok {
		return all, nil
	}

	colNames := c.ColNames

	val := reflect.ValueOf(i)
	var whereParts []string
	var valParts []interface{}
	for walk, maxLen := 0, e.NumField(); walk < maxLen; walk++ {
		f := e.Field(walk)
		s := val.Field(walk)
		switch f.Type.Name() {
		case "int64":
			num := s.Int()
			if num != 0 {
				whereParts = append(whereParts, fmt.Sprintf("`"+f.Name+"` = %d", num))
			}
		case "string":
			sval := s.String()
			if sval != "" {
				whereParts = append(whereParts, "`"+f.Name+"` = ?")
				valParts = append(valParts, sval)
			}
		}
	}

	var sqlStmt string
	if len(whereParts) > 0 {
		sqlStmt = "SELECT " + strings.Join(colNames, ", ") + " FROM `" + e.Name() + "` WHERE " + strings.Join(whereParts, " AND ")
	} else {
		sqlStmt = "SELECT " + strings.Join(colNames, ", ") + " FROM `" + e.Name() + "`"
	}

	if strings.HasPrefix(ord, "order by") {
		sqlStmt = fmt.Sprintf("%s %s", sqlStmt, orderClause)
	} else if orderClause != "" {
		sqlStmt = fmt.Sprintf("%s ORDER BY %s", sqlStmt, orderClause)
	}
	stmt, err := conn.Prepare(sqlStmt)
	if err != nil {
		return nil, errors.New("statement (prepare ; SelectAll) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = stmt.Close() }()
	rows, err := stmt.Query(valParts...)
	if err != nil {
		return nil, errors.New("statement (query ; SelectAll) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = rows.Close() }()

	var results []interface{}
	var ids []int64
	typ := reflect.TypeOf(i)
	orig := reflect.New(typ)
	el := reflect.Indirect(orig)
	for rows.Next() {
		var values []interface{}

		for idx := range colNames {
			f := el.Field(idx)
			v := f.Addr().Interface()
			values = append(values, v)
		}

		if err = rows.Scan(values...); err != nil {
			return nil, errors.New("problem scanning results sql: " + sqlStmt + " err: " + err.Error())
		}
		id := el.FieldByName("Id").Int()
		ids = append(ids, id)
		v := el.Interface()
		c.Cacher.Set(id, v)
		results = append(results, v)
	}
	c.Cacher.SetAll(ord, ids)

	return results, err
}

func ExistsTable(db *sql.DB, i interface{}) (bool, error) {
	if i == nil {
		return false, errors.New("nil interface in ExistsTable")
	}

	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}
	sqlStmt := "DESC `" + e.Name() + "`"
	stmt, err := db.Prepare(sqlStmt)
	if err != nil {
		return false, errors.New("statement (prepare) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = stmt.Close() }()
	rows, err := stmt.Query()
	if err != nil {
		return false, errors.New("statement (query) " + sqlStmt + " " + err.Error())
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return true, nil
	}
	return false, nil
}
