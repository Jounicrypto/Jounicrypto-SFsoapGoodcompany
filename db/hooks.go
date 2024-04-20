package db

//Validator can be used to augment types that should have validations rules that need to be enforced only on specific
//times during their lifecycle, such as on creation and on updates.
type Validator interface {
	PreCreate(interface{}, *Validation) error
	PreUpdate(interface{}, *Validation) error
}

//PostHooker is to be run after a new instance of an object of any instance that implements this is saved to the
//database.
type PostHooker interface {
	PostCreate(i interface{})
	PostUpdate(i interface{})
}
