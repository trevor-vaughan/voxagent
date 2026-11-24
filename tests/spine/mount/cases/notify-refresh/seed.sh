mkdir -p /mnt/x /mnt/y /mnt/z
printf "tmpfs\t/mnt/x\ttmpfs\tdefaults\t0\t0\ntmpfs\t/mnt/y\ttmpfs\tdefaults\t0\t0\ntmpfs\t/mnt/z\ttmpfs\tdefaults\t0\t0\n" > /etc/fstab
mount -t tmpfs tmpfs /mnt/x
mount -t tmpfs tmpfs /mnt/z
