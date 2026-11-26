# age with the default type, atime: only the old file goes.
tidy { '/srv/at': age => '1d', recurse => true }
