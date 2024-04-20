package db

type Index struct {
	Name     string
	Col      string
	Unique   bool
	Fulltext bool
}

type TableInit interface {
	Indexes() []Index
}

type TableVersion struct {
	Id   int64
	Name string
	Hash string
}

func (_ TableVersion) Indexes() []Index {
	return []Index{{Col: "Name"}}
}

func getIndices(i interface{}) []Index {
	var result []Index
	if dbInit, ok := i.(TableInit); ok {
		result = dbInit.Indexes()
	}
	return result
}
