<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * @property int $id
 * @property string $request_id
 * @property string $sku
 * @property string $user_id
 * @property int $qty
 */
class Order extends Model
{
    // created_at is filled by the column default; there is no updated_at.
    public $timestamps = false;

    protected $fillable = ['request_id', 'sku', 'user_id', 'qty'];

    protected function casts(): array
    {
        return [
            'qty' => 'integer',
        ];
    }
}
