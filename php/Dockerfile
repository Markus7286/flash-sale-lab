# Stage 1: resolve vendor/ on the same PHP minor the runtime uses.
FROM php:8.4-cli-alpine AS vendor
COPY --from=composer:2 /usr/bin/composer /usr/bin/composer
COPY --from=mlocati/php-extension-installer:2 /usr/bin/install-php-extensions /usr/local/bin/
# package:discover boots the app, and config/database.php names Pdo\Pgsql constants.
RUN install-php-extensions pdo_pgsql && apk add --no-cache git unzip
WORKDIR /app

COPY composer.json composer.lock ./
RUN composer install --no-dev --no-interaction --no-progress --no-scripts --no-autoloader --prefer-dist

COPY . .
RUN composer dump-autoload --no-dev --classmap-authoritative \
 && php artisan package:discover

# Stage 2: nginx in front of php-fpm, the way Laravel is usually deployed.
FROM php:8.4-fpm-alpine
COPY --from=mlocati/php-extension-installer:2 /usr/bin/install-php-extensions /usr/local/bin/
RUN install-php-extensions pdo_pgsql opcache \
 && apk add --no-cache nginx \
 && rm /usr/local/etc/php-fpm.d/*.conf

COPY php-fpm.conf /usr/local/etc/php-fpm.d/flashsale.conf
COPY opcache.ini /usr/local/etc/php/conf.d/zz-flashsale.ini
COPY nginx.conf /etc/nginx/nginx.conf

WORKDIR /app
COPY --from=vendor --chown=www-data:www-data /app /app

ENV APP_ENV=production \
    APP_DEBUG=false \
    LOG_CHANNEL=stderr \
    FPM_WORKERS=25

EXPOSE 8090
ENTRYPOINT ["/app/docker-entrypoint.sh"]
