<?php

namespace App\FlashSale;

enum Status: string
{
    case Reserved = 'reserved';
    // The request_id was already applied and stock was left untouched.
    case Duplicate = 'duplicate';
    case SoldOut = 'sold_out';
    case UserLimit = 'user_limit';
    case UnknownSku = 'unknown_sku';

    // 409 for sold-out and per-user-limit: the request was well formed, the
    // current state refused it.
    public function httpCode(): int
    {
        return match ($this) {
            self::Reserved => 201,
            self::Duplicate => 200,
            self::SoldOut, self::UserLimit => 409,
            self::UnknownSku => 404,
        };
    }
}
