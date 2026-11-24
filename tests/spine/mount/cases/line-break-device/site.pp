mount { "/mnt/a": ensure => defined, device => "/dev/x\n/dev/y", fstype => "ext4", options => "defaults" }
