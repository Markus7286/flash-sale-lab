<?php

use Illuminate\Support\Facades\Artisan;
use Illuminate\Support\Facades\DB;

// The schema is internal/flashsale/schema.sql, mounted read-only, not a Laravel
// migration: both servers must run against byte-identical tables and indexes.
Artisan::command('flashsale:schema {path=/srv/schema.sql}', function (string $path) {
    $sql = file_get_contents($path);
    if ($sql === false) {
        $this->error("cannot read {$path}");

        return 1;
    }

    DB::transaction(function () use ($sql) {
        // migrateLockID in postgres.go, so this never races the Go api applying the same file.
        DB::select('SELECT pg_advisory_xact_lock(72860301)');
        DB::unprepared($sql); // @phpstan-ignore argument.type (running this file is the command's whole job)
    }, attempts: 1);
    $this->info("applied {$path}");

    return 0;
})->purpose('Apply the schema the Go binaries apply at startup');
