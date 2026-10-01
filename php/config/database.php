<?php

use Pdo\Pgsql;

return [
    'default' => 'pgsql',

    'connections' => [
        // No search_path or timezone on purpose: Laravel sends a SET for each one on
        // every new connection, a round trip the Go api never makes.
        'pgsql' => [
            'driver' => 'pgsql',
            'url' => env('DATABASE_URL'),
            'charset' => 'utf8',
            'prefix' => '',
            'options' => [
                // The PHP side of pgxpool: each FPM worker keeps its connection across
                // requests instead of paying a Postgres backend fork per request.
                PDO::ATTR_PERSISTENT => (bool) env('DB_PERSISTENT', true),
                // Native prepares cost PDO a separate Parse round trip per query, where pgx
                // caches the statement; PQexecParams is one round trip, as pgx is once warm.
                Pgsql::ATTR_DISABLE_PREPARES => (bool) env('DB_DISABLE_PREPARES', true),
            ],
        ],
    ],
];
