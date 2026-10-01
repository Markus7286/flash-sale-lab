#!/bin/sh
set -eu

# Config is cached here rather than at build time because it bakes in the
# environment, and DATABASE_URL only exists once compose has started us.
php artisan flashsale:schema
php artisan optimize

php-fpm &
nginx -g 'daemon off;' &

# Either process exiting takes the container down, rather than leaving nginx
# answering 502 in front of a dead pool.
wait -n
exit 1
