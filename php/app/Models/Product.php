<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * @property string $sku
 * @property int $stock
 * @property int $per_user_limit
 */
class Product extends Model
{
    protected $primaryKey = 'sku';

    protected $keyType = 'string';

    public $incrementing = false;

    // created_at is filled by the column default; there is no updated_at.
    public $timestamps = false;

    protected function casts(): array
    {
        return [
            'stock' => 'integer',
            'per_user_limit' => 'integer',
        ];
    }
}
