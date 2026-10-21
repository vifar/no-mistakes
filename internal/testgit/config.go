package testgit

import "os"

func ClearConfigEnv() {
	for _, key := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		if err := os.Unsetenv(key); err != nil {
			panic(err)
		}
	}
}
