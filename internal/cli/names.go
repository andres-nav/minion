package cli

import "math/rand/v2"

var nameAdjectives = []string{
	"amber", "brave", "brisk", "calm", "clever", "cosmic", "crimson", "daring",
	"eager", "electric", "fuzzy", "gentle", "golden", "happy", "hidden", "jolly",
	"keen", "lively", "lucky", "mellow", "merry", "mighty", "nimble", "noble",
	"quiet", "rapid", "rustic", "sage", "sharp", "silent", "silver", "solar",
	"spry", "stellar", "sunny", "swift", "tidy", "vivid", "witty", "zesty",
}

var nameNouns = []string{
	"otter", "falcon", "maple", "badger", "comet", "cobra", "dingo", "eagle",
	"ferret", "gecko", "heron", "ibis", "jaguar", "koala", "lemur", "lynx",
	"marmot", "newt", "ocelot", "panda", "quokka", "raven", "robin", "salmon",
	"tapir", "urchin", "viper", "walrus", "yak", "zebra", "bison", "crane",
	"dove", "egret", "finch", "goose", "hawk", "kiwi", "mole", "puma",
}

// randomName returns a friendly Docker-style "adjective_noun" job id.
func randomName() string {
	return nameAdjectives[rand.IntN(len(nameAdjectives))] + "_" + nameNouns[rand.IntN(len(nameNouns))]
}
