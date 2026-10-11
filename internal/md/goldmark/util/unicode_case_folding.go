package util

var unicodeCaseFoldings map[rune][]rune

func init() {
	unicodeCaseFoldings = make(map[rune][]rune, _unicodeCaseFoldingLength)
	cTo := 0
	for i := range _unicodeCaseFoldingLength {
		tTo := cTo + int(_unicodeCaseFoldingToIndex[i])
		to := _unicodeCaseFoldingTo[cTo:tTo]
		unicodeCaseFoldings[_unicodeCaseFoldingFrom[i]] = to
		cTo = tTo
	}
}
