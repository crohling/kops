package nodetasks

import (
	"fmt"
	"github.com/golang/glog"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/nodeup/cloudinit"
	"k8s.io/kops/upup/pkg/fi/nodeup/local"
	"k8s.io/kops/upup/pkg/fi/utils"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const FileType_Symlink = "symlink"
const FileType_Directory = "directory"
const FileType_File = "file"

type File struct {
	Path     string
	Contents fi.Resource

	Mode        *string `json:"mode"`
	IfNotExists bool    `json:"ifNotExists"`

	OnChangeExecute []string `json:"onChangeExecute,omitempty"`

	Symlink *string `json:"symlink,omitempty"`
	Owner   *string `json:"owner,omitempty"`
	Group   *string `json:"group,omitempty"`
	Type    string  `json:"type"`
}

var _ fi.Task = &File{}
var _ fi.HasDependencies = &File{}

func NewFileTask(name string, src fi.Resource, destPath string, meta string) (*File, error) {
	f := &File{
		//Name:     name,
		Contents: src,
		Path:     destPath,
	}

	if meta != "" {
		err := utils.YamlUnmarshal([]byte(meta), f)
		if err != nil {
			return nil, fmt.Errorf("error parsing meta for file %q: %v", name, err)
		}
	}

	if f.Symlink != nil && f.Type == "" {
		f.Type = FileType_Symlink
	}

	return f, nil
}

var _ fi.HasDependencies = &File{}

func (f *File) GetDependencies(tasks map[string]fi.Task) []fi.Task {
	var deps []fi.Task
	if f.Owner != nil {
		ownerTask := tasks["user/"+*f.Owner]
		if ownerTask == nil {
			glog.Fatalf("Unable to find task %q", "user/"+*f.Owner)
		}
		deps = append(deps, ownerTask)
	}

	// Depend on disk mounts
	// For simplicity, we just depend on _all_ disk mounts
	// We could check the mountpath, but that feels excessive...
	for _, v := range tasks {
		if _, ok := v.(*MountDiskTask); ok {
			deps = append(deps, v)
		}
	}

	return deps
}

func (f *File) String() string {
	return fmt.Sprintf("File: %q", f.Path)
}

func findFile(p string) (*File, error) {
	stat, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
	}

	actual := &File{}
	actual.Path = p
	actual.Mode = fi.String(fi.FileModeToString(stat.Mode() & os.ModePerm))

	uid := int(stat.Sys().(*syscall.Stat_t).Uid)
	owner, err := fi.LookupUserById(uid)
	if err != nil {
		return nil, err
	}
	if owner != nil {
		actual.Owner = fi.String(owner.Name)
	} else {
		actual.Owner = fi.String(strconv.Itoa(uid))
	}

	gid := int(stat.Sys().(*syscall.Stat_t).Gid)
	group, err := fi.LookupGroupById(gid)
	if err != nil {
		return nil, err
	}
	if group != nil {
		actual.Group = fi.String(group.Name)
	} else {
		actual.Group = fi.String(strconv.Itoa(gid))
	}

	if (stat.Mode() & os.ModeSymlink) != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return nil, fmt.Errorf("error reading symlink target: %v", err)
		}

		actual.Type = FileType_Symlink
		actual.Symlink = fi.String(target)
	} else if (stat.Mode() & os.ModeDir) != 0 {
		actual.Type = FileType_Directory
	} else {
		actual.Type = FileType_File
		actual.Contents = fi.NewFileResource(p)
	}

	return actual, nil
}

func (e *File) Find(c *fi.Context) (*File, error) {
	actual, err := findFile(e.Path)
	if err != nil {
		return nil, err
	}
	if actual == nil {
		return nil, nil
	}

	// To avoid spurious changes
	actual.IfNotExists = e.IfNotExists
	if e.IfNotExists {
		actual.Contents = e.Contents
	}
	actual.OnChangeExecute = e.OnChangeExecute

	return actual, nil
}

func (e *File) Run(c *fi.Context) error {
	return fi.DefaultDeltaRunMethod(e, c)
}

func (s *File) CheckChanges(a, e, changes *File) error {
	return nil
}

func (_ *File) RenderLocal(t *local.LocalTarget, a, e, changes *File) error {
	dirMode := os.FileMode(0755)
	fileMode, err := fi.ParseFileMode(fi.StringValue(e.Mode), 0644)
	if err != nil {
		return fmt.Errorf("invalid file mode for %q: %q", e.Path, fi.StringValue(e.Mode))
	}

	if a != nil {
		if e.IfNotExists {
			glog.V(2).Infof("file exists and IfNotExists set; skipping %q", e.Path)
			return nil
		}
	}

	changed := false
	if e.Type == FileType_Symlink {
		if changes.Symlink != nil {
			// This will currently fail if the target already exists.
			// That's probably a good thing for now ... it is hard to know what to do here!
			glog.Infof("Creating symlink %q -> %q", e.Path, *changes.Symlink)
			err := os.Symlink(*changes.Symlink, e.Path)
			if err != nil {
				return fmt.Errorf("error creating symlink %q -> %q: %v", e.Path, *changes.Symlink, err)
			}
			changed = true
		}
	} else if e.Type == FileType_Directory {
		if a == nil {
			parent := filepath.Dir(strings.TrimSuffix(e.Path, "/"))
			err := os.MkdirAll(parent, dirMode)
			if err != nil {
				return fmt.Errorf("error creating parent directories %q: %v", parent, err)
			}

			err = os.MkdirAll(e.Path, fileMode)
			if err != nil {
				return fmt.Errorf("error creating directory %q: %v", e.Path, err)
			}
			changed = true
		}
	} else if e.Type == FileType_File {
		if changes.Contents != nil {
			err = fi.WriteFile(e.Path, e.Contents, fileMode, dirMode)
			if err != nil {
				return fmt.Errorf("error copying file %q: %v", e.Path, err)
			}
			changed = true
		}
	} else {
		return fmt.Errorf("File type=%q not valid/supported", e.Type)
	}

	if changes.Mode != nil {
		modeChanged, err := fi.EnsureFileMode(e.Path, fileMode)
		if err != nil {
			return fmt.Errorf("error changing mode on %q: %v", e.Path, err)
		}
		changed = changed || modeChanged
	}

	if changes.Owner != nil || changes.Group != nil {
		ownerChanged, err := fi.EnsureFileOwner(e.Path, fi.StringValue(e.Owner), fi.StringValue(e.Group))
		if err != nil {
			return fmt.Errorf("error changing owner/group on %q: %v", e.Path, err)
		}
		changed = changed || ownerChanged
	}

	if changed && e.OnChangeExecute != nil {
		args := e.OnChangeExecute
		human := strings.Join(args, " ")

		glog.Infof("Changed; will execute OnChangeExecute command: %q", human)

		cmd := exec.Command(args[0], args[1:]...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("error executing command %q: %v\nOutput: %s", human, err, output)
		}
	}

	return nil
}

func (_ *File) RenderCloudInit(t *cloudinit.CloudInitTarget, a, e, changes *File) error {
	dirMode := os.FileMode(0755)
	fileMode, err := fi.ParseFileMode(fi.StringValue(e.Mode), 0644)
	if err != nil {
		return fmt.Errorf("invalid file mode for %q: %q", e.Path, e.Mode)
	}

	if e.Type == FileType_Symlink {
		t.AddCommand(cloudinit.Always, "ln", "-s", fi.StringValue(e.Symlink), e.Path)
	} else if e.Type == FileType_Directory {
		parent := filepath.Dir(strings.TrimSuffix(e.Path, "/"))
		t.AddCommand(cloudinit.Once, "mkdir", "-p", "-m", fi.FileModeToString(dirMode), parent)
		t.AddCommand(cloudinit.Once, "mkdir", "-m", fi.FileModeToString(dirMode), e.Path)
	} else if e.Type == FileType_File {
		err = t.WriteFile(e.Path, e.Contents, fileMode, dirMode)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("File type=%q not valid/supported", e.Type)
	}

	if e.Owner != nil || e.Group != nil {
		t.Chown(e.Path, fi.StringValue(e.Owner), fi.StringValue(e.Group))
	}

	if e.OnChangeExecute != nil {
		t.AddCommand(cloudinit.Always, e.OnChangeExecute...)
	}

	return nil
}
