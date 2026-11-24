mkdir -p /etc/modprobe.d; printf 'blacklist udf\nblacklist cramfs\n' > /etc/modprobe.d/blacklist.conf
