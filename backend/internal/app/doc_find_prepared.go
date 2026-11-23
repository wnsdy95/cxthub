package app

const docFindPreparedWindows = 8

type docDisplayPreparationRequired struct {
}

func (failure *docDisplayPreparationRequired) Error() string {
	return "display preparation is required"
}
