package db

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"log"
	"reflect"
	"strings"
)

type Seeder interface {
	Seed(conn *sql.DB, c *Crud) error
}

func dropTable(conn *sql.DB, i interface{}) error {
	var name string
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		name = reflect.TypeOf(i).Elem().Name()
	} else {
		name = reflect.TypeOf(i).Name()
	}

	_, err := conn.Query("DROP TABLE `" + name + "`")
	if err != nil {
		return err
	}
	return nil
}

func genTable(conn *sql.DB, i interface{}) []string {
	var result []string

	var name string
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		name = reflect.TypeOf(i).Elem().Name()
	} else {
		name = reflect.TypeOf(i).Name()
	}

	result = append(result, genCreateTableStmt(i))
	result = append(result, genCreateIndexStmts(name, i)...)

	if err := checkIfTableExists(conn, i); err != nil {
		log.Println(err, "; attempting to create tables and indices")
		return result
	} else {
		s := "SELECT Id, Hash FROM TableVersion WHERE Name = ?"
		stmt, err := conn.Prepare(s)
		if err != nil || stmt == nil {
			log.Fatal("Could not prepare select for DbVersion ", s, " ", err)
		}

		res, err := stmt.Query(name)
		if err != nil {
			log.Fatal("Could not exec stmt ", s, " ", name, " ", err)
		}
		var h, id string
		if res.Next() {
			err = res.Scan(&id, &h)
			if err != nil {
				log.Fatal("Could not fetch column ", s, " ", err)
			}
			sha := sha256.New()
			for _, st := range result {
				_, _ = sha.Write([]byte(st))
			}
			encoder := base64.Encoding{}
			hash := encoder.EncodeToString(sha.Sum(nil))
			if hash != h {
				return result
			}
		}
	}
	return []string{}
}

func genCreateTableStmt(i interface{}) string {
	if i == nil {
		return ""
	}

	pk := ""
	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	var strCols []string
	var txtCols []string
	var numCols []string

	for walk, maxLen := 0, e.NumField(); walk < maxLen; walk++ {
		f := e.Field(walk)
		if f.Name == "Created" || f.Name == "Updated" {
			continue
		}
		typeName := f.Type.String()
		switch typeName {
		case "string":
			name := f.Name
			if strings.HasSuffix(name, "Txt") {
				txtCols = append(txtCols, "`"+name+"` TEXT NOT NULL")
			} else {
				strCols = append(strCols, "`"+name+"` VARCHAR(255) NOT NULL")
			}
		case "int64":
			name := f.Name
			if "Id" == name {
				pk = "Id BIGINT NOT NULL AUTO_INCREMENT"
			} else {
				numCols = append(numCols, "`"+name+"` BIGINT NOT NULL")
			}
		case "float64":
			name := f.Name
			numCols = append(numCols, "`"+name+"` DOUBLE PRECISION NOT NULL")
		case "*time.Time":
			name := f.Name
			numCols = append(numCols, "`"+name+"` DATETIME")
		case "time.Time":
			name := f.Name
			numCols = append(numCols, "`"+name+"` DATETIME NOT NULL")
		case "bool":
			name := f.Name
			numCols = append(numCols, "`"+name+"` BOOL NOT NULL")
		default:
			name := f.Name
			numCols = append(numCols, "`"+name+"` BIGINT NOT NULL")
		}
	}

	var cols []string
	cols = append(cols, numCols...)
	cols = append(cols, strCols...)
	cols = append(cols, txtCols...)

	var res bytes.Buffer
	res.Write([]byte("CREATE TABLE `" + e.Name() + "` ("))
	res.Write([]byte(pk + ", "))
	res.WriteString("Created DATETIME DEFAULT CURRENT_TIMESTAMP, Updated DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,")
	res.Write([]byte(strings.Join(cols, ", ")))
	res.Write([]byte(",PRIMARY KEY (Id)"))
	res.Write([]byte(") CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"))
	return res.String()
}

func genCreateIndexStmts(name string, i interface{}) []string {
	var res []string
	for _, idx := range getIndices(i) {
		var stmt string
		if idx.Fulltext {
			stmt = ""
		} else if idx.Unique {
			stmt = "CREATE UNIQUE INDEX `" + idx.Name + "idx` ON `" + name + "` (" + idx.Col + ");"
		} else {
			if idx.Name != "" {
				stmt = "CREATE INDEX `" + idx.Name + "idx` ON `" + name + "` (" + idx.Col + ");"
			} else {
				stmt = "CREATE INDEX `" + idx.Col + "idx` ON `" + name + "` (" + idx.Col + ");"
			}
		}
		res = append(res, stmt)
	}
	return res
}

func checkIfTableExists(db *sql.DB, i interface{}) error {
	var e reflect.Type
	if reflect.TypeOf(i).Kind() == reflect.Ptr {
		e = reflect.TypeOf(i).Elem()
	} else {
		e = reflect.TypeOf(i)
	}

	rows, err := db.Query("DESC TABLE `" + e.Name() + "`;")
	if err != nil {
		return err
	}
	if rows != nil && rows.Next() {
		_ = rows.Close()
		return nil
	}
	_ = rows.Close()
	return nil
}
