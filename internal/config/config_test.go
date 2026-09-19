package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/client/internal/config"
)

func TestAFreshClientHasDefaultsAndRemembersWhatItIsTold(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := filepath.Join(t.TempDir(), "Pacenote")

	c, err := config.Load(dir)
	r.NoError(err)
	r.Equal(config.Default(), c)
	r.False(c.Paired())

	c.Token = "tok-1"
	c = c.WithSetting("voice", "volume", "70").WithSetting("voice", "mode", "on").WithSetting("engineer", "language", "es")
	r.NoError(config.Save(dir, c))
	back, err := config.Load(dir)
	r.NoError(err)
	r.Equal("tok-1", back.Token)
	r.True(back.Paired())
	r.Equal("70", back.Setting("voice", "volume"))
	r.Equal("es", back.Setting("engineer", "language"))
	r.Empty(back.Setting("voice", "speed"))
	r.Empty(back.Setting("nobody", "x"))
	gone := back.WithSetting("voice", "volume", "").WithSetting("voice", "mode", "")
	r.Nil(gone.Plugins["voice"], "a plugin with nothing kept is not listed")
	r.Equal("70", back.Setting("voice", "volume"), "the original is untouched")
	r.Nil(config.Default().WithSetting("p", "k", "").Plugins)

	if runtime.GOOS != "windows" {
		var info os.FileInfo
		info, err = os.Stat(filepath.Join(dir, config.FileName))
		r.NoError(err)
		r.Equal(os.FileMode(0o600), info.Mode().Perm(), "the token is this user's")
		var entries []os.DirEntry
		entries, err = os.ReadDir(dir)
		r.NoError(err)
		r.Len(entries, 1, "no temporary file is left behind")
	}

	r.NoError(os.WriteFile(filepath.Join(dir, config.FileName), []byte("{not json"), 0o600))
	_, err = config.Load(dir)
	r.ErrorContains(err, "not the JSON it should be")

	_, err = config.DefaultDir()
	r.NoError(err)

	// A directory that cannot be created, one that cannot be written, and a
	// file that is a directory.
	blocked := filepath.Join(t.TempDir(), "file")
	r.NoError(os.WriteFile(blocked, nil, 0o600))
	r.Error(config.Save(filepath.Join(blocked, "under-a-file"), config.Default()))
	if runtime.GOOS != "windows" {
		readOnly := filepath.Join(t.TempDir(), "ro")
		r.NoError(os.MkdirAll(readOnly, 0o500))
		r.ErrorContains(config.Save(readOnly, config.Default()), "writing")
	}
	isDir := t.TempDir()
	r.NoError(os.MkdirAll(filepath.Join(isDir, config.FileName), 0o700))
	r.ErrorContains(config.Save(isDir, config.Default()), "writing")
	_, err = config.Load(isDir)
	r.ErrorContains(err, "reading")
}

func TestTheDefaultDirectoryNeedsAHome(t *testing.T) {
	r := require.New(t)
	dir, err := config.DefaultDir()
	r.NoError(err)
	r.Equal("Pacenote", filepath.Base(dir))
	if runtime.GOOS == "windows" {
		return
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	_, err = config.DefaultDir()
	r.Error(err)
}
