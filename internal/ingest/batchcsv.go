package ingest

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// batchRequiredColumns 是批次清单必须有的列。
//
// 批次号只由冷链台账给出，本系统不造新号：所以这份清单只用于
// 「把在库批次挂到判定链上」，不让任何一方在这里补写或改写批次号。
var batchRequiredColumns = []string{
	"point_id", "batch_ref", "product_name", "quantity", "expiry_date",
}

// LoadBatchFile 读取在库批次清单 CSV，返回批次列表。
//
// 表头驱动（不写死列序）：台账侧导出的表头顺序不保证与本系统一致，
// 按列名取才对得上。任一行不合法即报错并带上行号：
// 批次清单少一行或多一行，在库货的数量就说不清，
// 而「说不清」会被后面的人当成「没问题」。
func LoadBatchFile(path string) ([]domain.Batch, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errcode.Newf(errcode.NotFound, "打不开批次清单文件 %s：%v", path, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, errcode.Newf(errcode.Validation, "批次清单文件 %s 读不出表头：%v", path, err)
	}
	index := make(map[string]int, len(header))
	for i, name := range header {
		index[strings.TrimSpace(strings.TrimPrefix(name, "\ufeff"))] = i
	}
	for _, required := range batchRequiredColumns {
		if _, ok := index[required]; !ok {
			return nil, errcode.Newf(errcode.Validation,
				"批次清单文件 %s 缺少必需列 %s", path, required)
		}
	}

	batches := make([]domain.Batch, 0)
	rowNo := 1
	for {
		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, errcode.Newf(errcode.Validation,
				"批次清单文件 %s 第 %d 行读不出来：%v", path, rowNo+1, err)
		}
		rowNo++
		if len(record) == 0 {
			continue
		}

		batch, err := batchFromRecord(record, index, rowNo, path)
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	return batches, nil
}

// batchFromRecord 把一行批次记录转成领域对象。
func batchFromRecord(record []string, index map[string]int, rowNo int, path string) (domain.Batch, error) {
	value := func(name string) string {
		i := index[name]
		if i >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[i])
	}

	batch := domain.Batch{
		PointID:     value("point_id"),
		BatchRef:    value("batch_ref"),
		ProductName: value("product_name"),
		ExpiryDate:  value("expiry_date"),
	}

	if batch.BatchRef == "" {
		return domain.Batch{}, errcode.Newf(errcode.Validation,
			"批次清单文件 %s 第 %d 行缺 batch_ref", path, rowNo)
	}
	if batch.ProductName == "" {
		return domain.Batch{}, errcode.Newf(errcode.Validation,
			"批次清单文件 %s 第 %d 行缺 product_name（批次 %s 挂不到任何温区上）",
			path, rowNo, batch.BatchRef)
	}

	quantity := value("quantity")
	if quantity == "" {
		return domain.Batch{}, errcode.Newf(errcode.Validation,
			"批次清单文件 %s 第 %d 行缺 quantity", path, rowNo)
	}
	q, err := strconv.Atoi(quantity)
	if err != nil || q < 0 {
		return domain.Batch{}, errcode.Newf(errcode.Validation,
			"批次清单文件 %s 第 %d 行 quantity 不是合法数量：%q", path, rowNo, quantity)
	}
	batch.Quantity = q

	return batch, nil
}

// BatchSummary 把批次清单汇总成一句话摘要，给命令行的 stderr 用。
func BatchSummary(batches []domain.Batch) string {
	if len(batches) == 0 {
		return "0 个在库批次"
	}
	products := make(map[string]int)
	for _, b := range batches {
		products[b.ProductName]++
	}
	parts := make([]string, 0, len(products))
	for name, n := range products {
		parts = append(parts, fmt.Sprintf("%s×%d", name, n))
	}
	// 排序保证同一份清单每次打印出来的摘要一致，便于比对。
	sort.Strings(parts)
	return fmt.Sprintf("%d 个在库批次（%s）", len(batches), strings.Join(parts, "、"))
}
