# Five entries against max_files => 3 fail generation; nothing goes.
tidy { '/srv/mx': recurse => true, max_files => 3 }
