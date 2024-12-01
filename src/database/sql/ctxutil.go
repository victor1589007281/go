// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sql

import (
	"context"
	"database/sql/driver"
	"errors"
)

// ctxDriverPrepare 处理带有上下文的预处理语句
// 参数:
// - ctx: 上下文对象，用于控制超时和取消
// - ci: 数据库连接接口
// - query: SQL查询语句
// 返回预处理语句对象和可能的错误
func ctxDriverPrepare(ctx context.Context, ci driver.Conn, query string) (driver.Stmt, error) {
	// 检查连接是否支持带上下文的预处理
	if ciCtx, is := ci.(driver.ConnPrepareContext); is {
		// 如果支持，直接使用带上下文的预处理方法
		return ciCtx.PrepareContext(ctx, query)
	}
	// 不支持则使用普通预处理方法
	si, err := ci.Prepare(query)
	if err == nil {
		select {
		default:
		case <-ctx.Done():
			// 如果上下文已取消，关闭语句并返回错误
			si.Close()
			return nil, ctx.Err()
		}
	}
	return si, err
}

// ctxDriverExec 执行带有上下文的SQL语句
// 参数:
// - ctx: 上下文对象
// - execerCtx: 支持上下文的执行器接口
// - execer: 标准执行器接口
// - query: SQL查询语句
// - nvdargs: 命名参数值数组
func ctxDriverExec(ctx context.Context, execerCtx driver.ExecerContext, execer driver.Execer, query string, nvdargs []driver.NamedValue) (driver.Result, error) {
	// 如果支持上下文执行器，直接使用
	if execerCtx != nil {
		return execerCtx.ExecContext(ctx, query, nvdargs)
	}
	// 将命名参数转换为普通参数
	dargs, err := namedValueToValue(nvdargs)
	if err != nil {
		return nil, err
	}

	// 检查上下文是否已取消
	select {
	default:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// 使用普通执行器执行查询
	return execer.Exec(query, dargs)
}

// ctxDriverQuery 执行带有上下文的查询操作
func ctxDriverQuery(ctx context.Context, queryerCtx driver.QueryerContext, queryer driver.Queryer, query string, nvdargs []driver.NamedValue) (driver.Rows, error) {
	// 如果支持上下文查询器，直接使用
	if queryerCtx != nil {
		return queryerCtx.QueryContext(ctx, query, nvdargs)
	}
	// 将命名参数转换为普通参数
	dargs, err := namedValueToValue(nvdargs)
	if err != nil {
		return nil, err
	}

	// 检查上下文是否已取消
	select {
	default:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// 使用普通查询器执行查询
	return queryer.Query(query, dargs)
}

// ctxDriverStmtExec 执行预处理语句的执行操作
func ctxDriverStmtExec(ctx context.Context, si driver.Stmt, nvdargs []driver.NamedValue) (driver.Result, error) {
	// 检查语句是否支持上下文执行
	if siCtx, is := si.(driver.StmtExecContext); is {
		return siCtx.ExecContext(ctx, nvdargs)
	}
	// 将命名参数转换为普通参数
	dargs, err := namedValueToValue(nvdargs)
	if err != nil {
		return nil, err
	}

	// 检查上下文是否已取消
	select {
	default:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// 使用普通方式执行预处理语句
	return si.Exec(dargs)
}

// ctxDriverStmtQuery 执行预处理语句的查询操作
func ctxDriverStmtQuery(ctx context.Context, si driver.Stmt, nvdargs []driver.NamedValue) (driver.Rows, error) {
	// 检查语句是否支持上下文查询
	if siCtx, is := si.(driver.StmtQueryContext); is {
		return siCtx.QueryContext(ctx, nvdargs)
	}
	// 将命名参数转换为普通参数
	dargs, err := namedValueToValue(nvdargs)
	if err != nil {
		return nil, err
	}

	// 检查上下文是否已取消
	select {
	default:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// 使用普通方式执行预处理查询
	return si.Query(dargs)
}

// ctxDriverBegin 开始一个数据库事务
func ctxDriverBegin(ctx context.Context, opts *TxOptions, ci driver.Conn) (driver.Tx, error) {
	// 检查连接是否支持带选项的事务开始
	if ciCtx, is := ci.(driver.ConnBeginTx); is {
		// 转换事务选项
		dopts := driver.TxOptions{}
		if opts != nil {
			dopts.Isolation = driver.IsolationLevel(opts.Isolation)
			dopts.ReadOnly = opts.ReadOnly
		}
		return ciCtx.BeginTx(ctx, dopts)
	}

	// 处理不支持带选项事务的情况
	if opts != nil {
		// Check the transaction level. If the transaction level is non-default
		// then return an error here as the BeginTx driver value is not supported.
		// 检查隔离级别，如果不是默认级别则返回错误
		if opts.Isolation != LevelDefault {
			return nil, errors.New("sql: driver does not support non-default isolation level")
		}

		// If a read-only transaction is requested return an error as the
		// BeginTx driver value is not supported.
		// 检查只读选项，如果要求只读则返回错误
		if opts.ReadOnly {
			return nil, errors.New("sql: driver does not support read-only transactions")
		}
	}

	// 如果上下文没有截止时间，直接开始事务
	if ctx.Done() == nil {
		return ci.Begin()
	}

	// 开始事务并检查上下文是否已取消
	txi, err := ci.Begin()
	if err == nil {
		select {
		default:
		case <-ctx.Done():
			// 如果上下文已取消，回滚事务
			txi.Rollback()
			return nil, ctx.Err()
		}
	}
	return txi, err
}

// namedValueToValue 将命名参数转换为普通参数值
func namedValueToValue(named []driver.NamedValue) ([]driver.Value, error) {
	// 创建相同长度的参数值数组
	dargs := make([]driver.Value, len(named))
	// 遍历命名参数
	for n, param := range named {
		// 如果存在参数名，返回错误（不支持命名参数）
		if len(param.Name) > 0 {
			return nil, errors.New("sql: driver does not support the use of Named Parameters")
		}
		// 只保留参数值
		dargs[n] = param.Value
	}
	return dargs, nil
}
