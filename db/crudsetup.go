package db

import (
	"reflect"
	"strings"
)

func genSelectByIdStmt(e reflect.Type) string {
	colNames := colNames(e)

	return "SELECT " + strings.Join(colNames, ", ") + " FROM `" + e.Name() + "` WHERE Id = ?"
}

func colNames(e reflect.Type) []string {
	var colNames []string
	for walk, maxLen := 0, e.NumField(); walk < maxLen; walk++ {
		f := e.Field(walk)
		colNames = append(colNames, "`"+f.Name+"`")
	}
	return colNames
}

func rawColNames(e reflect.Type) []string {
	var colNames []string
	for walk, maxLen := 0, e.NumField(); walk < maxLen; walk++ {
		f := e.Field(walk)
		colNames = append(colNames, f.Name)
	}
	return colNames
}

func genDeleteStmt(e reflect.Type) string {
	var softDelete, deleteon bool
	for walk, maxLen := 0, e.NumField(); walk < maxLen; walk++ {
		n := strings.ToLower(e.Field(walk).Name)
		if "deleted" == n {
			softDelete = true
		}
		if "deletedon" == n {
			deleteon = true
		}
	}

	if softDelete && deleteon {
		return "UPDATE `" + e.Name() + "` SET Deleted = 'true' WHERE Id = ?"
	} else if softDelete {
		return "UPDATE `" + e.Name() + "` SET Deleted = 'true' AND DeletedOn = NOW() WHERE Id = ?"
	}

	return "DELETE FROM `" + e.Name() + "` WHERE Id = ?"
}

func genInsertStmt(e reflect.Type) string {
	var colNames []string
	var questionMarks []string
	for walk, maxLen := 0, e.NumField(); walk < maxLen; walk++ {
		f := e.Field(walk)
		if f.Name == "Id" {
			continue
		} else {
			colNames = append(colNames, "`"+f.Name+"`")
			questionMarks = append(questionMarks, "?")
		}
		// else if f.Name == "Created" || f.Name == "Updated" {
		// 	colNames = append(colNames, "`"+f.Name+"`")
		// 	questionMarks = append(questionMarks, "NOW()")
		// }

	}

	return "INSERT INTO `" + e.Name() + "` (" + strings.Join(colNames, ", ") + ") VALUES (" + strings.Join(questionMarks, ", ") + ")"
}

func genUpdateStmt(e reflect.Type) string {
	var colNames []string
	for walk, maxLen := 0, e.NumField(); walk < maxLen; walk++ {
		f := e.Field(walk)
		if f.Name == "Id" {
			continue
		} else {
			colNames = append(colNames, "`"+f.Name+"` = ?")
		}
		// else if f.Name == "Updated" {
		// 	colNames = append(colNames, "`"+f.Name+"` = NOW()")
		// } else if f.Name == "Created" {
		// 	continue
		// }

	}

	return "UPDATE `" + e.Name() + "` SET " + strings.Join(colNames, ", ") + " WHERE Id = ?"
}
