package main

import (
	"context"
	"fmt"
	"github.com/cloudwego/kitex/pkg/klog"
	"math/rand"
	"strconv"
	"sync"
	"time"
	"word-count/cmd/keyby/pack"
	"word-count/config"
	"word-count/kitex_gen/keybydemo"
	"word-count/pkg/errno"
)

// KeybyServiceImpl implements the last service interface defined in the IDL.
type KeybyServiceImpl struct{}

var (
	keybyNum      int
	maxNum        int
	keybyIdx      int
	keybyChanList []chan *keybydemo.CreateKeybyRequest
	keyIdx        int
	windowFunc    string
	windowFilter  string
	windowAssign  int64
)

func init() {
	rand.Seed(time.Now().UnixNano())
	// 由于是单进程模型多协程，因此上游的负载均衡策略本质上就是发往下游哪个算子，这里对应就是选择哪个map协程处理上游source的数据
	keyIdx = config.GlobalDAGConfig.GetInt("keyby.key")
	keybyNum = config.GlobalDAGConfig.GetInt("keyby.num")
	windowFunc = config.GlobalDAGConfig.GetString("window.func")
	windowFilter = config.GlobalDAGConfig.GetString("window.filter")
	windowAssign = config.GlobalDAGConfig.GetInt64("window.assign")
	maxNum = config.GetConfig().Keyby.Num
	// DAG中map算子数量非法状态
	if keybyNum <= 0 || keybyNum > maxNum {
		klog.Fatal("DAG中指定的keyby数量过小/过大")
	}
	keybyIdx = 0
	keybyChanList = make([]chan *keybydemo.CreateKeybyRequest, 0)
	for i := 1; i <= keybyNum; i++ {
		ch := make(chan *keybydemo.CreateKeybyRequest, 1)
		keybyChanList = append(keybyChanList, ch)
		go func(chan *keybydemo.CreateKeybyRequest, int) {
			// reduce服务客户端初始化
			//rpc.InitReduceRPC()
			// TODO 分区初始化，应该是map结构，然后不断更新当前内容（目前是累加，然后定期清空，暂时不考虑retract语义）
			// TODO 需要一个计时器，定期获取发送过来的数据的时间戳,根据window的参数配置，创建窗口，进行水位线的定时获取
			// TODO 为每个逻辑分区给一个watermark，然后取一个最小值，后续小于该水位线的数据到达则无效不统计
			timeMap := make(map[string]time.Time)
			keyMap := make(map[string]int64)
			distinctMap := make(map[int64]int64)
			var minWaterMark *time.Time
			var lock sync.Mutex

			for {
				// 获取map算子发过来的数据，如果没有消息会被阻塞
				msg, _ := <-ch
				// TODO 开启一个协程去处理累加和更新分区的数据，并且需要一个多协程共享的计时器，同时需要实现retract

				go func(*keybydemo.CreateKeybyRequest) {
					key := msg.Content[keyIdx]
					value := msg.Value
					timeNowStamp, _ := strconv.ParseInt(msg.TimeStamp, 10, 64)
					timeNow := time.Unix(timeNowStamp, 0)

					_, ok := timeMap[key]
					// 没有数据插入则以当前时间为起点watermark，并且初始化计时器
					if !ok {
						timeMap[key] = timeNow
						klog.Info(fmt.Sprintf("更新当前算子 %v 的分区为 %v 的局部waterMark 为 %v", keybyIdx, key, timeNow))

						if minWaterMark == nil {
							minWaterMark = &timeNow
							klog.Info(fmt.Sprintf("更新当前算子 %d 的waterMark 为 %v", keybyIdx, timeNow))
						} else {
							// 保持单个算子的水位线为所有key分区的最小值
							if minWaterMark.After(timeNow) {
								minWaterMark = &timeNow
								klog.Info(fmt.Sprintf("更新当前算子 %d 的waterMark 为 %v", keybyIdx, timeNow))
							}
						}
					}
					// 早于水位线则丢弃
					if minWaterMark.After(timeNow) {
						klog.Info("丢弃数据: ", key, value, timeNow)
					} else {
						//timeGap, _ := time.ParseDuration("+30s")
						//nextWaterMark := minWaterMark.Add(timeGap)

						lock.Lock()
						keyNum := keyMap[key]
						keyMap[key]++
						klog.Info(fmt.Sprintf("key为：%s 的单词出现了 %v 次", key, keyNum+1))
						if keyNum > 0 {
							distinctMap[keyNum]--
							klog.Info(fmt.Sprintf("retract <==== 词频为 %v 的单词有 %v 个", keyNum, distinctMap[keyNum]))
						}
						distinctMap[keyNum+1]++
						klog.Info(fmt.Sprintf("add ====> 词频为 %v 的单词有 %v 个", keyNum+1, distinctMap[keyNum+1]))
						lock.Unlock()

						//// 如果是位于水位线与窗口之间，则算在当前窗口
						//if nextWaterMark.After(timeNow) {
						//	lock.Lock()
						//	keyNum := keyMap[key]
						//	keyMap[key]++
						//	klog.Info(fmt.Sprintf("key为：%s 的单词出现了 %v 次", key, keyNum+1))
						//	distinctMap[keyNum]--
						//	klog.Info(fmt.Sprintf("retract ====> 词频为 %v 的单词有 %v 个", keyNum, distinctMap[keyNum]))
						//	distinctMap[keyNum+1]++
						//	klog.Info(fmt.Sprintf("add ====> 词频为 %v 的单词有 %v 个", keyNum+1, distinctMap[keyNum+1]))
						//	lock.Unlock()
						//} else {
						//	// TODO 应该将数据放入后一个窗口(这里可以考虑新建窗口，并将数据发送到下游算子)
						//
						//}
					}

				}(msg)
				// TODO 对应并发修改map（关于retract，是否需要加锁，如果可以保证语义正确，不加锁是最好的，加锁则无所谓retract还是直接添加了）
				// TODO 这里需要retract的原因是sum会维护一个map表，统计每个单词30秒内出现的次数，且distinct规则也是去统计这个表（在它访问时如果map被并发修改了，则会出现问题（可能是1/可能是2，倒不一定是1，2都存在，但是并发问题无法完全预测））
				// TODO distinct是随着窗口统计的，但是统计的时候，可能map表正在修改
				// TODO 考虑到高并发访问，sum表和distinct表的改动都需要加锁

				// TODO 统计数据，并且按照窗口间隔请求reduce服务，将统计数据发送给reduce

				klog.Info(fmt.Sprintf("当前处理的是keyby算子%d号，消息内容为%s", keybyIdx, msg))

			}
		}(ch, keyIdx)
	}
}

func HandleKeybyMsg(req *keybydemo.CreateKeybyRequest) {
	// 需要根据keyIdx对应的值进行hash为不同分区，然后需要分发到对应keyby算子
	key := req.Content[keyIdx]
	keybyIdx = int(key[0]) % keybyNum
	keybyChanList[keybyIdx] <- req
}

// CreateKeyby implements the KeybyServiceImpl interface.
func (s *KeybyServiceImpl) CreateKeyby(ctx context.Context, req *keybydemo.CreateKeybyRequest) (resp *keybydemo.CreateKeybyResponse, err error) {
	//klog.Info(req.Content, req.TimeStamp, req.Value)

	// TODO 根据key的位置，选择key值，并且根据k进行hash，分发给两个keyby协程算子，并且每个算子维护m个滚动窗口（每个窗口有时间段之分），并且结合水位线，此时在keyby算子上就能实现5分钟的窗口聚合
	// TODO 同时数据不断发送给下游reduce，进行retract的归约（是否也需要按照窗口的呃分钟进行一次？还是说是每个单词来了归一次）

	HandleKeybyMsg(req)

	resp = new(keybydemo.CreateKeybyResponse)
	resp.BaseResp = pack.BuildBaseResp(errno.Success)
	return resp, nil
}
